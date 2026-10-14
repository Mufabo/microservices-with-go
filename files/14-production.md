# Chapter 14 --- Going to Production: Scaling, Secrets, and Resilience

Snapvault is deployed on a self-healing cluster, provisioned from code, and observable from end to end. That is a real system --- but "it runs" and "it's ready for production traffic" are different claims. Production means a surge of uploads on a Saturday afternoon doesn't tip the thumbnailer over. It means a database password isn't sitting in a manifest. It means that when the metadata service has a bad ten minutes, users still see their feed instead of an error page, and that the broker holding the whole system together isn't one Pod away from taking everything down. This final build chapter hardens Snapvault along exactly those lines: it scales on the signals we collected in Chapter 13, manages secrets properly, makes services and the broker resilient to partial failure, and finally delivers the gateway aggregation we've deferred since Chapter 4.

## Scaling with the load

Two replicas of each service is a fine starting point and the wrong permanent answer --- traffic isn't constant. Kubernetes scales workloads with the **HorizontalPodAutoscaler** (HPA), which watches a metric and adjusts replica count to keep it near a target. For the HTTP services, CPU is a reasonable signal: more concurrent requests means more CPU, so target a comfortable utilization and let the HPA add Pods under load and remove them when it subsides. Create `k8s/hpa-gateway.yaml`:

``` yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: gateway
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: gateway
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 65
```

The HPA keeps average CPU near 65%, scaling between two and ten Pods. On Autopilot, those extra Pods pull in extra capacity automatically --- the cluster grows under you with no node management.

The thumbnailer is a different kind of workload. Its load isn't requests; it's a backlog of messages. The good news is that resizing images is CPU-bound, so a busy worker *is* a hot worker, and CPU works as a first signal here too. Create `k8s/hpa-thumbnails.yaml`:

``` yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: thumbnails
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: thumbnails
  minReplicas: 1
  maxReplicas: 8
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
```

Because the thumbnailer is a competing consumer on a durable JetStream consumer (Chapter 6), every new Pod immediately shares the load with no coordination. This is the moment the asynchronous design pays its biggest dividend: a traffic spike becomes a queue that grows, the busy workers drive the autoscaler, more workers spin up, the queue drains, and the workers spin back down --- all without a human, and without the gateway or upload path ever slowing down. The work was decoupled from the request precisely so it could absorb a surge like this.

The most direct signal of all would be the **backlog itself**: CPU only tells you the workers are busy, while the pending count tells you how far behind they are. An HPA can scale on it as an `External` metric, but that takes two pieces of plumbing Snapvault doesn't install. First, something has to export the count: the prometheus-nats-exporter from Chapter 13. Second, the cluster needs the **Custom Metrics Stackdriver Adapter**, which makes Cloud Monitoring metrics visible to the HPA. With both in place, the metric block becomes:

``` yaml
  metrics:
    - type: External
      external:
        metric:
          name: prometheus.googleapis.com|jetstream_consumer_num_pending|gauge
          selector:
            matchLabels:
              metric.labels.consumer_name: thumbnailer
        target:
          type: AverageValue
          averageValue: "30"      # pending messages per worker
```

Check the exact metric and label names against your exporter's `/metrics` output before relying on it; they vary with the exporter's flags. The companion repository ships the CPU version and keeps this block in a comment.

::: note
Scaling has a floor and a ceiling for good reasons. `minReplicas: 1` on the worker keeps one consumer always attached so new uploads are processed promptly even at rest; a `minReplicas: 0` "scale to zero" is possible but adds cold-start latency to the first upload after a quiet period. `maxReplicas` is a cost and blast-radius guardrail --- without a ceiling, a runaway producer (or a bug republishing events) could scale you into a very large bill.
:::

## Secrets, the production way

In Chapter 9 we created a Kubernetes Secret imperatively to get moving, and flagged that it was a starting point, not a destination. The problem with a plain Secret is that the credential lives in the cluster, base64-encoded, for anyone with the right RBAC to read, and it's disconnected from rotation --- change the database password and you must remember to update the Secret. The production pattern is to keep the value in **Google Secret Manager** (which we declared in Terraform in Chapter 11) and let Pods read it at runtime through workload identity, so it's never stored in the cluster at all.

The cleanest mechanism on GKE is the **Secret Manager CSI driver**, which mounts secrets as files into the Pod. You declare which secrets a Pod needs with a `SecretProviderClass`:

``` yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: snapvault-secrets
spec:
  provider: gcp
  parameters:
    secrets: |
      - resourceName: "projects/PROJECT_ID/secrets/snapvault-database-url/versions/latest"
        path: "database-url"
      - resourceName: "projects/PROJECT_ID/secrets/snapvault-mongo-uri/versions/latest"
        path: "mongo-uri"
```

The Pod mounts this as a volume --- say at `/secrets` --- and the values appear as files the service reads at startup: `/secrets/database-url` and `/secrets/mongo-uri`. Grant the `snapvault-runtime` Google service account we bound via workload identity in Chapter 11 `roles/secretmanager.secretAccessor` on each secret (one `google_secret_manager_secret_iam_member` per secret in Terraform), so access is the Pod's own identity --- no key, no in-cluster copy, and a rotated secret in Secret Manager flows to new Pods on their next start. A small change to `config` reads a value from a file when a `*_FILE` variant is set, falling back to the plain environment variable for local development:

``` go
// FileOrEnv returns the contents of the file named by key+"_FILE" if that
// variable is set, otherwise the value of key. Lets the same binary read a
// CSI-mounted secret in the cluster and a plain env var on a laptop.
func FileOrEnv(key string) string {
    if path := os.Getenv(key + "_FILE"); path != "" {
        if b, err := os.ReadFile(path); err == nil {
            return strings.TrimSpace(string(b))
        }
    }
    return os.Getenv(key)
}
```

Both services that hold a secret switch to it: the feed service calls `config.FileOrEnv("DATABASE_URL")`, and the metadata service calls `config.FileOrEnv("MONGO_URI")` in place of the `config.MustString("MONGO_URI")` it used until now. Because `FileOrEnv` returns an empty string when neither variable is set, metadata checks for that and exits, keeping the fail-fast behavior `MustString` gave it.

That switch is easy to forget for the *next* secret someone adds, and forgetting fails silently: a service that reads its secret with plain `MustString` never looks at the mounted file. So close the trap in `config` itself and make `MustString` file-aware too:

``` go
func MustString(key string) string {
    if v := FileOrEnv(key); v != "" {
        return v
    }
    panic("config: required environment variable " + key + " (or " + key + "_FILE) is not set")
}
```

Now a required value is read the same way whichever helper a service uses, and the code paths can't drift apart. In the cluster, `DATABASE_URL_FILE=/secrets/database-url` and `MONGO_URI_FILE=/secrets/mongo-uri` point at the mounted files; on a laptop, `DATABASE_URL` and `MONGO_URI` hold the local connection strings. The code stays portable; only the source of the secret changes.

## Resilience: surviving partial failure

A distributed system fails in a way a monolith can't: *partially*. The metadata service can be down while everything else is up. The network between the gateway and the images service can hiccup for two seconds. A monolith is all-up or all-down; a microservice system is usually mostly-up, and "mostly-up" must not mean "completely broken for the user." Three habits get you most of the way: bounded waits, careful retries, and graceful degradation.

**Every outbound call needs a timeout.** Our HTTP *server* timeouts were set in Chapter 2, but a service making calls to other services needs *client* deadlines too, or one slow dependency stalls a goroutine indefinitely and the stalls pile up until the caller falls over. Give every cross-service call a context deadline:

``` go
ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
defer cancel()
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL+"/metadata/"+id, nil)
```

**Retries help, but only for the right operations.** A transient network blip or a momentary 503 is worth retrying with exponential backoff and jitter; a 400 is not (it'll fail identically every time), and retrying a *non-idempotent* operation risks doing it twice. This is where Chapter 6's discipline pays off: every write a *consumer* makes is idempotent --- `Upsert` in metadata, `INSERT ... ON CONFLICT DO NOTHING` and a repeatable `UPDATE` in feed --- which is exactly what lets JetStream redeliver freely. Reads such as the gateway's calls to feed and metadata are safe to retry too. The upload is the exception: each `POST /images` mints a new ID, so retrying a request that actually succeeded stores the photo twice. Make that safe with a client-supplied idempotency key, or don't retry it. A small helper captures the pattern:

``` go
// retry runs fn up to attempts times with exponential backoff and jitter,
// stopping early if fn returns nil or ctx is cancelled.
func retry(ctx context.Context, attempts int, fn func() error) error {
    var err error
    for i := 0; i < attempts; i++ {
        if err = fn(); err == nil {
            return nil
        }
        backoff := time.Duration(1<<i) * 100 * time.Millisecond
        jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
        select {
        case <-time.After(backoff + jitter):
        case <-ctx.Done():
            return ctx.Err()
        }
    }
    return err
}
```

The jitter matters more than it looks: without it, a hundred Pods that failed at the same instant all retry at the same instant, hammering a recovering dependency in synchronized waves --- the "thundering herd." Spreading retries randomly lets the dependency actually recover.

**Graceful degradation** is the habit that most distinguishes a resilient system. When a non-essential dependency is unavailable, return a useful partial answer instead of a hard failure. This is the right way to finally build the gateway aggregation we deferred in Chapter 4.

## Finishing the gateway aggregation

Until now the gateway has mostly proxied single requests to single services. The aggregation pattern is its real job: serve a composed view by calling several services and merging the results --- and, crucially, still returning something useful if one of those calls fails. Snapvault's image-detail view needs the feed entry (with its thumbnail) and the metadata (tags, dimensions). Those live in two services. The gateway fans out to both concurrently, each with its own timeout, and merges what comes back:

The gateway needs the shape of both responses, but it must not import the feed or metadata services' Go types --- that would couple their code, which the whole design avoids. Instead it declares just the fields it reads, in `services/gateway/aggregate.go`. It depends on the JSON shape, the same contract any external client depends on:

``` go
// The gateway decodes only the fields it needs and deliberately does not
// import the feed or metadata packages.
type feedItem struct {
    ID          string `json:"id"`
    Owner       string `json:"owner"`
    ThumbObject string `json:"thumb_object,omitempty"`
    Width       int    `json:"width,omitempty"`
    Height      int    `json:"height,omitempty"`
    UploadedAt  string `json:"uploaded_at"`
}

type imageMeta struct {
    ContentType  string   `json:"content_type"`
    OriginalName string   `json:"original_name"`
    Size         int64    `json:"size"`
    Tags         []string `json:"tags,omitempty"`
}

// GET /api/images/{id} — aggregate feed + metadata into one view,
// degrading gracefully if metadata is unavailable.
func (g *Gateway) imageDetail(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    log := httpx.LoggerFrom(r.Context(), g.log)

    var (
        wg      sync.WaitGroup
        fi      *feedItem
        feedErr error
        meta    *imageMeta
        metaOK  bool
    )

    // Feed is essential: if it fails, the whole view fails.
    wg.Add(1)
    go func() {
        defer wg.Done()
        fi, feedErr = g.fetchFeed(r.Context(), id)
    }()

    // Metadata is enrichment: it gets its own short deadline, and if it
    // fails we serve the view without it.
    wg.Add(1)
    go func() {
        defer wg.Done()
        ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
        defer cancel()
        m, err := g.fetchMeta(ctx, id)
        if err != nil {
            log.Warn("metadata unavailable; degrading", "id", id, "err", err)
            return
        }
        meta, metaOK = m, true
    }()

    wg.Wait()

    if feedErr != nil {
        http.Error(w, "not found", http.StatusNotFound)
        return
    }

    resp := map[string]any{
        "id":           fi.ID,
        "owner":        fi.Owner,
        "thumb_object": fi.ThumbObject,
        "width":        fi.Width,
        "height":       fi.Height,
        "uploaded_at":  fi.UploadedAt,
    }
    if metaOK {
        resp["content_type"] = meta.ContentType
        resp["original_name"] = meta.OriginalName
        resp["size"] = meta.Size
        resp["tags"] = meta.Tags
        resp["metadata_status"] = "ok"
    } else {
        resp["metadata_status"] = "unavailable"
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(resp)
}
```

`fetchFeed` and `fetchMeta` are thin wrappers that `GET` `FEED_URL + "/feed/" + id` and `METADATA_URL + "/metadata/" + id`, forward the `X-Request-Id` from Chapter 13, and decode the JSON. They use an `http.Client` with an `otelhttp` transport and an overall 10-second timeout.

Two things make this production-grade rather than just concurrent. First, the two calls run in parallel, so the response is as slow as the *slower* dependency, not the *sum* --- latency that matters under load. Second, and more importantly, the two failures are treated differently by design: the feed is essential, so its failure is a 404; metadata is enrichment, so its failure produces a slightly thinner response with an honest `metadata_status` field, not an error. (One refinement worth making in your own code: treating *every* feed failure as a 404 tells a client "this image doesn't exist" when the truth may be "the feed is down." Distinguishing the feed's own 404 from a timeout or 5xx, and answering the latter with a 502 or 503, keeps the error honest too.) A Snapvault user whose metadata service is having a bad minute still sees their photo and their feed --- they just don't see tags for a little while, and most won't notice. That deliberate classification of dependencies into essential and optional is the heart of graceful degradation, and it's a decision only you can make, per endpoint.

::: warning
Graceful degradation is a choice you must make explicitly; it is never the default. The default behavior of "call a service, propagate its error" turns every dependency into a hard dependency, so one wobbling service fails every request that touches it. Go through your aggregating endpoints and decide, for each downstream call, whether it is essential (fail) or enrichment (degrade). Writing that decision into the code, as above, is what keeps a partial outage partial.
:::

## A broker that isn't a single point of failure

In Chapter 9 we ran NATS as a single Pod. That was honest for getting started and dishonest as a production posture: the broker is the system's nervous system, and one Pod is one failure away from severing it. Production NATS runs as a **cluster** --- three nodes --- with JetStream streams **replicated** across them, so the loss of a node loses neither the broker nor the messages it was holding. The Helm chart makes the cluster a values change. Since Chapter 12 the `nats` release belongs to ArgoCD, so the change is a commit to the `valuesObject` of the `nats` Application:

``` yaml
      valuesObject:
        config:
          cluster:
            enabled: true
            replicas: 3
          jetstream:
            enabled: true
            fileStore:
              pvc: { size: 10Gi }
```

(If you're still managing the release by hand, the equivalent is `helm upgrade nats nats/nats --namespace snapvault --reuse-values --set config.cluster.enabled=true --set config.cluster.replicas=3`.) The release name doesn't change, so neither does `NATS_URL`.

The matching application change is one field on the stream. Our `natsutil.EnsureStream` from Chapter 6 created the `IMAGES` stream with a single replica; for production it asks for three:

``` go
_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
    Name:     events.StreamName,
    Subjects: subjects,
    Storage:  jetstream.FileStorage,
    Replicas: 3, // survive the loss of one node
})
```

A stream can't have more replicas than there are servers, so this exact code would fail against the single-node NATS in your Compose stack. Drive the number from configuration instead --- a `STREAM_REPLICAS` setting that defaults to 1 locally and is 3 in the production ConfigMap. (The companion repository leaves the line commented out, with that note, so it runs unchanged against a single node.)

With three replicas, a published event is committed to a quorum before `Publish` returns, and a node failure neither drops in-flight messages nor stops new ones. Combined with our connection options from Chapter 6 --- `MaxReconnects(-1)` and a reconnect wait --- a service rides out a broker node restart without losing work: it reconnects to a surviving node, and JetStream redelivers anything unacknowledged.

## Safe rollouts and right-sized Pods

Two smaller pieces finish the hardening. A **PodDisruptionBudget** tells Kubernetes the minimum availability to preserve during voluntary disruptions --- a node upgrade, a scale-down --- so it never drains every gateway Pod at once:

``` yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: gateway
spec:
  minAvailable: 1
  selector:
    matchLabels: { app: gateway }
```

And **resource requests** --- already required by Autopilot --- deserve a deliberate pass now that you have real latency and CPU data from Chapter 13's metrics. Set requests from observed usage, not guesswork: too low and Pods get throttled or evicted under load; too high and you pay for idle capacity, since Autopilot bills on requests. Observability isn't only for incidents; it's the input to right-sizing, and right-sizing is most of cost control on Autopilot, where you pay per Pod for exactly what you request.

## What we didn't build: identity

One production requirement this book deliberately leaves out deserves to be named plainly: **Snapvault has no authentication.** Anyone who can reach the gateway can upload, and `owner` is whatever the client types into a form field. That's fine for learning the architecture, and not fine for anything public. The gateway is where it belongs: verify a token there --- an OIDC ID token from an identity provider, or a session cookie --- derive `owner` from the verified identity instead of the request body, and pass it to the images service in a header that internal services trust *only* from the gateway. Add rate limiting per user at the same point, alongside the upload size limit from Chapter 5. None of this changes the architecture, which is exactly why the gateway exists: it's the one place the change has to be made.

## The system we built

It's worth stepping back, because we've come a long way from Chapter 1. Snapvault began as the *idea* of a community photo platform and became five small services --- a gateway, an uploader, a thumbnailer, a metadata store, a feed --- each owning its data, each independently deployable, talking through a mix of direct HTTP calls and asynchronous NATS events. We containerized them identically, ran them together with Compose, gave each the database its job actually wanted, connected them with a durable event stream, tested them against real infrastructure, shipped them through a keyless CI/CD pipeline, ran them on a self-healing GKE cluster, provisioned that cluster from Terraform, deployed them continuously with GitOps, made the whole thing observable, and --- in this chapter --- taught it to scale with load, guard its secrets, and stay useful when parts of it fail.

But the most important lesson is the one from Chapter 1, and it's worth repeating at the end where it lands differently: none of this is free, and none of it is automatically the right choice. Every capability in this book is also a cost --- more moving parts, more failure modes, more operational surface than a single well-built Go binary would have. Microservices earned their place in Snapvault because the workloads genuinely differ: a thumbnailer that must scale independently of uploads, a read-heavy feed that wants a different database than a write-heavy metadata store, background work that must not slow the request path. When those forces are real, the architecture in this book is a powerful answer. When they aren't --- when a monolith would serve you fine --- the most senior move is to build the monolith, keep its internal boundaries clean, and split out a service only when a specific, present pressure makes the split pay for itself. The goal was never "microservices." The goal was a system that fits its problem, and now you have the tools to build that whether the answer is five services or one.

### Summary

-   The **HorizontalPodAutoscaler** scales the HTTP services and the CPU-bound thumbnailer on **CPU**, so a traffic spike becomes a self-draining queue, never a slow upload path. Scaling on **queue depth** directly is possible with an `External` metric, given the NATS exporter and the Custom Metrics Stackdriver Adapter.
-   Manage secrets with **Google Secret Manager** and the **CSI driver**, read at runtime via workload identity, so credentials never live in the cluster; a `*_FILE` config fallback keeps the same binary portable to a laptop.
-   Give every **outbound call a timeout**; **retry only idempotent operations** (Snapvault's consumer writes and reads are; the upload `POST` isn't) with exponential backoff **and jitter** to avoid thundering herds.
-   **Graceful degradation** is an explicit, per-endpoint choice: classify each downstream dependency as essential (fail) or enrichment (serve a partial answer), and write that decision into the code.
-   The **gateway aggregation** fans out to feed and metadata concurrently with independent timeouts, returns as slow as the slower call rather than their sum, and degrades when the optional dependency is down.
-   Run **NATS as a 3-node cluster with replicated JetStream streams** (`Replicas: 3`) so neither the broker nor its messages depend on a single Pod.
-   A **PodDisruptionBudget** preserves availability during voluntary disruptions; set **resource requests from observed metrics**, which is most of cost control on Autopilot.
-   Snapvault has **no authentication**; a public deployment needs identity verified at the gateway, with `owner` derived from it, plus per-user rate limits.
-   The deepest lesson is Chapter 1's: this architecture is a cost as much as a capability --- adopt it when the workloads genuinely demand it, and build the monolith when they don't.

There is no next chapter to bridge to --- Snapvault is built, deployed, observable, scalable, and resilient. What remains is reference material: the appendix collects the commands used throughout the book in one place, so the tools are at your fingertips as you build systems of your own.
