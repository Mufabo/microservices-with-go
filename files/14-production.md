# Chapter 14 --- Going to Production: Scaling, Secrets, Resilience, and Identity

Snapvault is deployed on a self-healing cluster, provisioned from code, and observable from end to end. That is a real system --- but "it runs" and "it's ready for production traffic" are different claims. Production means a surge of uploads on a Saturday afternoon doesn't tip the thumbnailer over. It means a database password isn't sitting in a manifest. It means that when the metadata service has a bad ten minutes, users still see their feed instead of an error page, and that the broker holding the whole system together isn't one Pod away from taking everything down. And it means knowing who uploaded each photo. This final build chapter hardens Snapvault along exactly those lines: it scales on the signals we collected in Chapter 13, manages secrets properly, makes services and the broker resilient to partial failure, delivers the gateway aggregation we've deferred since Chapter 4, serves the gateway over HTTPS, and puts real user authentication behind it.

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

The cleanest mechanism on GKE is the **Secret Manager add-on**, a Google-managed CSI driver that mounts secrets as files into the Pod. (Autopilot doesn't let you install the open-source Secrets Store CSI driver yourself, because it needs a privileged DaemonSet; the add-on is the supported way.) Turn it on with one block on the cluster in `infra/gke.tf`:

``` hcl
  secret_manager_config {
    enabled = true
  }
```

You declare which secrets a Pod needs with a `SecretProviderClass`. It names the project, so it belongs in the prod overlay rather than the base:

``` yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: snapvault-secrets
spec:
  provider: gke
  parameters:
    secrets: |
      - resourceName: "projects/PROJECT_ID/secrets/snapvault-database-url/versions/latest"
        path: "database-url"
      - resourceName: "projects/PROJECT_ID/secrets/snapvault-mongo-uri/versions/latest"
        path: "mongo-uri"
```

Each Pod that needs a secret mounts a CSI volume backed by that class, and is told where the file is. In the feed Deployment:

``` yaml
      containers:
        - name: feed
          env:
            - name: DATABASE_URL_FILE
              value: /secrets/database-url
          volumeMounts:
            - name: secrets
              mountPath: /secrets
              readOnly: true
      volumes:
        - name: secrets
          csi:
            driver: secrets-store-gke.csi.k8s.io
            readOnly: true
            volumeAttributes:
              secretProviderClass: snapvault-secrets
```

Metadata gets the same volume with `MONGO_URI_FILE=/secrets/mongo-uri`. The add-on reads each secret as the Pod's own Kubernetes service account, `snapvault` in the `snapvault` namespace, through Workload Identity, so that identity needs `roles/secretmanager.secretAccessor` on each secret --- no key, no in-cluster copy, and a rotated secret in Secret Manager reaches new Pods on their next start. In Terraform the grant names the service account as a Workload Identity principal:

``` hcl
data "google_project" "this" {}

resource "google_secret_manager_secret_iam_member" "feed_db" {
  secret_id = google_secret_manager_secret.database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/snapvault/sa/snapvault"
}
```

The companion repository's `infra/secrets.tf` makes one such grant per mounted secret, and declares the `snapvault-mongo-uri` secret next to Chapter 11's database URL. With every credential mounted this way, the `snapvault-secrets` Kubernetes Secret from Chapter 9 and the `secretRef` lines that injected it can be deleted: nothing in the cluster holds a credential any more.

On the code side, a small change to `config` reads a value from a file when a `*_FILE` variant is set, falling back to the plain environment variable for local development:

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

Switching each service to it is easy to forget for the *next* secret someone adds, and forgetting fails silently: a service that reads its secret with plain `MustString` never looks at the mounted file. So close the trap in `config` itself and make `MustString` file-aware too:

``` go
func MustString(key string) string {
    if v := FileOrEnv(key); v != "" {
        return v
    }
    panic("config: required environment variable " + key + " (or " + key + "_FILE) is not set")
}
```

Now a required value is read the same way whichever helper a service uses. The feed service reads `config.MustString("DATABASE_URL")`, so it fails fast if the secret is missing rather than handing `pgx` an empty connection string, and the metadata service reads `config.FileOrEnv("MONGO_URI")` and exits if that comes back empty. In the cluster, `DATABASE_URL_FILE` and `MONGO_URI_FILE` point at the mounted files; on a laptop, `DATABASE_URL` and `MONGO_URI` hold the local connection strings. The code stays portable; only the source of the secret changes.

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

    switch {
    case errors.Is(feedErr, errUpstreamNotFound):
        http.Error(w, "not found", http.StatusNotFound) // the image doesn't exist
        return
    case feedErr != nil:
        log.Error("feed unavailable", "id", id, "err", feedErr)
        http.Error(w, "feed unavailable", http.StatusBadGateway) // it may well exist
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

`fetchFeed` and `fetchMeta` are thin wrappers that `GET` `FEED_URL + "/feed/" + url.PathEscape(id)` and the matching metadata URL, forward the `X-Request-Id` from Chapter 13, and decode the JSON. They share a `getJSON` helper that turns an upstream `404` into the sentinel error `errUpstreamNotFound` and any other non-`200` into an ordinary error. They use an `http.Client` with an `otelhttp` transport and an overall 10-second timeout. The `PathEscape` matters: `r.PathValue` returns the *decoded* segment, so a request for `/api/images/..%2Fmetrics` would otherwise send the feed service a path the gateway never meant to call.

Two things make this production-grade rather than just concurrent. First, the two calls run in parallel, so the response is as slow as the *slower* dependency, not the *sum* --- latency that matters under load. Second, and more importantly, the two failures are treated differently by design: the feed is essential, so its failure fails the request; metadata is enrichment, so its failure produces a slightly thinner response with an honest `metadata_status` field, not an error. Even the essential failure is reported honestly: only the feed's own `404` becomes a `404`. A timeout or a 5xx becomes a `502 Bad Gateway`, because "the feed is down" is not "this image doesn't exist," and a client (or a cache) that hears the second will draw the wrong conclusion. A Snapvault user whose metadata service is having a bad minute still sees their photo and their feed --- they just don't see tags for a little while, and most won't notice. That deliberate classification of dependencies into essential and optional is the heart of graceful degradation, and it's a decision only you can make, per endpoint.

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

## HTTPS at the edge

Since Chapter 9 the gateway has been a `LoadBalancer` Service on port 80: plain HTTP. That was tolerable while nothing secret crossed it. The next section changes that --- every upload will carry a bearer token, and a bearer token works for whoever holds it, so sending it in the clear hands it to anyone on the path. Production traffic needs TLS first.

We don't terminate TLS in the gateway's Go code. GKE's **Ingress** creates a global external HTTPS load balancer in front of a Service, and a **ManagedCertificate** has Google issue and renew the certificate for your domain. The gateway Service becomes a plain `ClusterIP` --- so there is no longer an HTTP path around the load balancer --- annotated for container-native load balancing, which sends traffic straight to Pod IPs and health-checks them with the readiness probe:

``` yaml
apiVersion: v1
kind: Service
metadata:
  name: gateway
  annotations:
    cloud.google.com/neg: '{"ingress": true}'
spec:
  type: ClusterIP
  selector: { app: gateway }
  ports:
    - port: 80
      targetPort: 8080
```

The Ingress, the certificate, and a **FrontendConfig** that redirects HTTP to HTTPS go in the prod overlay, since the domain is per environment:

``` yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: gateway
  annotations:
    kubernetes.io/ingress.class: gce
    kubernetes.io/ingress.global-static-ip-name: snapvault-gateway-ip
    networking.gke.io/managed-certificates: gateway
    networking.gke.io/v1beta1.FrontendConfig: gateway
spec:
  defaultBackend:
    service:
      name: gateway
      port:
        number: 80
---
apiVersion: networking.gke.io/v1
kind: ManagedCertificate
metadata:
  name: gateway
spec:
  domains:
    - snapvault.example.com
---
apiVersion: networking.gke.io/v1beta1
kind: FrontendConfig
metadata:
  name: gateway
spec:
  redirectToHttps:
    enabled: true
```

The load balancer claims a static IP by name, which Terraform reserves so the address outlives any one Ingress:

``` hcl
resource "google_compute_global_address" "gateway" {
  name = "snapvault-gateway-ip"
}
```

Point your domain's DNS `A` record at that address (the companion repository exposes it as the `gateway_ip` output). Google issues the certificate only once the domain resolves to the load balancer, so expect the first certificate to take a while; `kubectl describe managedcertificate gateway` shows its progress. Add the same domain to Identity Platform's authorized domains, so users can sign in from it.

## Who's uploading: identity at the gateway

Since Chapter 6, `owner` has been whatever the client typed into a form field. Anyone who can reach the gateway can upload, as anyone. That was fine while we learned the architecture, and it's not fine for anything public. This is the change the gateway was built for back in Chapter 4: authentication happens in one place, at the edge, and nothing behind it has to learn how.

### Who signs users in

Snapvault doesn't store passwords, and shouldn't. Signing users in is the job of an **identity provider**, and on Google Cloud the one for an app's *public* users is **Identity Platform**, the managed form of Firebase Authentication. Users sign in with Google, email and password, or other providers, and the app receives an **ID token**: a JWT, signed by Google, that says who the user is.

::: note
**Identity Platform, not Cloud Identity.** The names are confusingly close. *Cloud Identity* manages an organization's own staff --- employee accounts, groups, their access to Google Cloud. *Identity Platform* handles sign-in for the people who use your app. Snapvault's users are the public, so Identity Platform is the one we want. If Snapvault were an internal tool for one company's employees, you'd use Cloud Identity accounts behind *Identity-Aware Proxy* instead, and write no sign-in code at all.
:::

Turning it on is more Terraform, in `infra/identity.tf`:

``` hcl
resource "google_project_service" "identitytoolkit" {
  service            = "identitytoolkit.googleapis.com"
  disable_on_destroy = false
}

resource "google_identity_platform_config" "default" {
  project = var.project_id

  sign_in {
    email {
      enabled           = true
      password_required = true
    }
  }

  # Web origins allowed to run the sign-in flow.
  authorized_domains = var.auth_authorized_domains

  depends_on = [google_project_service.identitytoolkit]
}
```

That enables email-and-password sign-in. "Sign in with Google" additionally needs an OAuth client, which you create in the console and register with a `google_identity_platform_default_supported_idp_config` resource.

The client side is the Firebase Auth SDK: the web or mobile app signs the user in, calls `getIdToken()`, and sends the token with every upload:

``` http
POST /api/images
Authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6...
```

Three claims in that token matter to us. `iss` is `https://securetoken.google.com/PROJECT_ID`, `aud` is `PROJECT_ID`, and `sub` is the user's stable ID. Tokens expire after an hour, and the SDK refreshes them on its own.

### Verifying tokens in the gateway

The gateway must check that a token really came from our project's Identity Platform and hasn't expired. That means checking the signature against Google's published keys, then the issuer, the audience, and the expiry. That's standard OpenID Connect, so we don't write any of it ourselves: `github.com/coreos/go-oidc` does it all from the issuer's discovery document. `services/gateway/auth.go` wraps it behind a one-method interface:

``` go
// authenticator resolves who is making a request.
type authenticator interface {
    UserID(r *http.Request) (string, error)
}

type oidcAuth struct{ verifier *oidc.IDTokenVerifier }

func newOIDCAuth(ctx context.Context, issuer, audience string) (*oidcAuth, error) {
    provider, err := oidc.NewProvider(ctx, issuer) // fetches discovery + keys
    if err != nil {
        return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
    }
    return &oidcAuth{verifier: provider.Verifier(&oidc.Config{ClientID: audience})}, nil
}

func (a *oidcAuth) UserID(r *http.Request) (string, error) {
    raw, ok := bearerToken(r) // the part after "Authorization: Bearer "
    if !ok {
        return "", errNoToken
    }
    tok, err := a.verifier.Verify(r.Context(), raw)
    if err != nil {
        return "", err
    }
    if tok.Subject == "" {
        return "", errors.New("token has no subject")
    }
    return tok.Subject, nil
}
```

Nothing in this code names Google. The provider is two settings, `AUTH_ISSUER` and `AUTH_AUDIENCE`, so moving to Auth0, Keycloak, or anything else that speaks OIDC is a configuration change.

A middleware puts it in front of the upload route. It turns a request that carries a token into one that carries a *user*:

``` go
// UserHeader (in internal/platform/httpx) carries the verified user ID from
// the gateway to internal services.
const UserHeader = "X-Snapvault-User"

func requireUser(auth authenticator, log *slog.Logger, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        user, err := auth.UserID(r)
        if err != nil {
            w.Header().Set("WWW-Authenticate", `Bearer realm="snapvault"`)
            http.Error(w, "authentication required", http.StatusUnauthorized)
            return
        }
        r.Header.Del("Authorization") // internal services never see the token
        r.Header.Set(httpx.UserHeader, user)
        next.ServeHTTP(w, r)
    })
}

// stripInternalHeaders runs on every request, before anything else.
func stripInternalHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        r.Header.Del(httpx.UserHeader)
        next.ServeHTTP(w, r)
    })
}
```

In `main`, only the upload route is wrapped. Reading the feed and the images stays public:

``` go
mux.Handle("POST /api/images", requireUser(auth, log, uploadProxy))
// ...
var h http.Handler = mux
h = stripInternalHeaders(h)
```

`stripInternalHeaders` is the line that makes the whole scheme sound. The images service will believe whatever `X-Snapvault-User` says, so a client must never be able to set it. Removing it from *every* inbound request means the only way that header reaches an internal service is by `requireUser` writing it, after a token has been verified.

The images service changes in the opposite direction: it stops reading `owner` from the form and refuses any upload that didn't come through that check:

``` go
owner := r.Header.Get(httpx.UserHeader)
if owner == "" {
    http.Error(w, "authentication required", http.StatusUnauthorized)
    return
}
```

### Local development, failing closed

There's no Identity Platform on your laptop, so the gateway has a development mode: set `AUTH_DEV_USER=dev-user` (Compose does) and every upload is attributed to that user without a token. The danger with any such switch is ending up in production by accident. So the gateway picks its mode **failing closed**:

- `AUTH_ISSUER` and `AUTH_AUDIENCE` set: real verification.
- Only `AUTH_DEV_USER` set: development mode, with a warning in the log at startup.
- Neither set: the gateway *refuses to start*.

A misconfigured deployment can't quietly become an open one. The prod overlay sets the issuer and audience (the `auth_issuer` and `auth_audience` Terraform outputs), and the base manifests set neither, so they can't run unconfigured.

### The trust boundary

Trusting a header is only safe if nobody but the gateway can send requests to the images service. The gateway is the only public entry point, and images is a `ClusterIP` Service, unreachable from outside the cluster. Inside the cluster, though, any Pod could call it directly and claim to be any user. A Kubernetes **NetworkPolicy** closes that gap. Once a policy selects a Pod, the Pod accepts only the traffic the policy lists, and everything else is dropped. Add `base/networkpolicy.yaml` to the config repo, and list it in `base/kustomization.yaml`:

``` yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: images-ingress
spec:
  podSelector:
    matchLabels: { app: images }
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels: { app: gateway }
      ports:
        - port: 8080
    - from:
        - namespaceSelector:
            matchLabels: { kubernetes.io/metadata.name: gmp-system }
      ports:
        - port: 8080
```

-   **The first rule admits the gateway.** A bare `podSelector` in `from` means Pods with that label in the policy's own namespace, so a Pod labelled `app: gateway` in some other namespace doesn't count.
-   **The second rule admits Managed Prometheus.** Its collectors run in the `gmp-system` namespace and scrape `/metrics` on the same port (Chapter 13). Leave them out and nothing fails loudly: the images service just stops reporting metrics. A NetworkPolicy works on addresses and ports, not URL paths, so it can't limit the collectors to `/metrics`.
-   **Health probes need no rule.** The kubelet probes from the node itself, and GKE allows node traffic to the node's own Pods, so readiness and liveness keep working.

GKE Autopilot clusters run GKE Dataplane V2, which enforces NetworkPolicy out of the box. On a cluster without an enforcing network plugin, the API server accepts the policy and nothing enforces it, so test it once. From a throwaway Pod with no `app: gateway` label, a request to `images:8080` should now time out, while the gateway keeps working.

The verification code is tested like any other code, without Google. `services/gateway/auth_test.go` starts a fake OIDC issuer --- a discovery document and a key set on an `httptest` server --- and signs tokens with its own key. It checks that a valid token is accepted, and that a wrong audience, wrong issuer, expired token, foreign signing key, or missing subject are all rejected. A forged `X-Snapvault-User` never reaches upstream. These are exactly the properties an attacker would probe, so they're worth pinning down in tests.

One more thing belongs at the same point: **per-user rate limits**, alongside the upload size limit from Chapter 5. Now that the gateway knows who is uploading, it can limit how much each user uploads.

## The system we built

It's worth stepping back, because we've come a long way from Chapter 1. Snapvault began as the *idea* of a community photo platform and became five small services --- a gateway, an uploader, a thumbnailer, a metadata store, a feed --- each owning its data, each independently deployable, talking through a mix of direct HTTP calls and asynchronous NATS events. We containerized them identically, ran them together with Compose, gave each the database its job actually wanted, connected them with a durable event stream, tested them against real infrastructure, shipped them through a keyless CI/CD pipeline, ran them on a self-healing GKE cluster, provisioned that cluster from Terraform, deployed them continuously with GitOps, made the whole thing observable, and --- in this chapter --- taught it to scale with load, guard its secrets, and stay useful when parts of it fail.

But the most important lesson is the one from Chapter 1, and it's worth repeating at the end where it lands differently: none of this is free, and none of it is automatically the right choice. Every capability in this book is also a cost --- more moving parts, more failure modes, more operational surface than a single well-built Go binary would have. Microservices earned their place in Snapvault because the workloads genuinely differ: a thumbnailer that must scale independently of uploads, a read-heavy feed that wants a different database than a write-heavy metadata store, background work that must not slow the request path. When those forces are real, the architecture in this book is a powerful answer. When they aren't --- when a monolith would serve you fine --- the most senior move is to build the monolith, keep its internal boundaries clean, and split out a service only when a specific, present pressure makes the split pay for itself. The goal was never "microservices." The goal was a system that fits its problem, and now you have the tools to build that whether the answer is five services or one.

### Summary

-   The **HorizontalPodAutoscaler** scales the HTTP services and the CPU-bound thumbnailer on **CPU**, so a traffic spike becomes a self-draining queue, never a slow upload path. Scaling on **queue depth** directly is possible with an `External` metric, given the NATS exporter and the Custom Metrics Stackdriver Adapter.
-   Manage secrets with **Google Secret Manager** and GKE's **Secret Manager add-on**, which mounts them as files read with the Pod's own workload identity, so credentials never live in the cluster; a `*_FILE` config fallback keeps the same binary portable to a laptop.
-   Give every **outbound call a timeout**; **retry only idempotent operations** (Snapvault's consumer writes and reads are; the upload `POST` isn't) with exponential backoff **and jitter** to avoid thundering herds.
-   **Graceful degradation** is an explicit, per-endpoint choice: classify each downstream dependency as essential (fail) or enrichment (serve a partial answer), and write that decision into the code.
-   The **gateway aggregation** fans out to feed and metadata concurrently with independent timeouts, returns as slow as the slower call rather than their sum, and degrades when the optional dependency is down.
-   Run **NATS as a 3-node cluster with replicated JetStream streams** (`Replicas: 3`) so neither the broker nor its messages depend on a single Pod.
-   A **PodDisruptionBudget** preserves availability during voluntary disruptions; set **resource requests from observed metrics**, which is most of cost control on Autopilot.
-   Put the gateway behind a GKE **Ingress** with a **ManagedCertificate** and an HTTP-to-HTTPS redirect, and make its Service `ClusterIP`, so tokens never cross the internet in the clear.
-   **Identity Platform** signs users in and issues OIDC ID tokens. The gateway verifies them (`go-oidc`: signature, issuer, audience, expiry), derives `owner` from the token, and passes it inward in a header it strips from every client request. The gateway fails closed: no auth configuration, no start. A **NetworkPolicy** lets only the gateway (and the metrics collectors) reach images, so no other Pod can forge that header.
-   The deepest lesson is Chapter 1's: this architecture is a cost as much as a capability --- adopt it when the workloads genuinely demand it, and build the monolith when they don't.

There is no next chapter to bridge to --- Snapvault is built, deployed, observable, scalable, and resilient. What remains is reference material: the appendix collects the commands used throughout the book in one place, so the tools are at your fingertips as you build systems of your own.
