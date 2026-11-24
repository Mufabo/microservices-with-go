# Chapter 9 --- Kubernetes on GKE Autopilot

Compose gave us the whole of Snapvault on one laptop: five services, a broker, two databases, all wired together by name. That is exactly what you want for development, and exactly what you do **not** want in production. Compose runs on a single host. If that host dies, Snapvault dies with it. If the images service crashes at 3 a.m., nothing restarts it. If a surge of uploads arrives, there is no way to run three copies of the thumbnailer instead of one without editing a file and re-running a command by hand. Production needs something that keeps services running, replaces them when they fail, rolls out new versions without downtime, and spreads the load across more than one machine. That something, for most teams, is Kubernetes.

This chapter takes Snapvault from Compose to a real cluster on **Google Kubernetes Engine (GKE)**, using its Autopilot mode. We won't cover all of Kubernetes --- that would be its own book --- but we will cover enough of the object model to deploy every Snapvault service, expose the gateway to the internet, run the broker, and watch the cluster heal itself. By the end you'll have the system running on infrastructure that survives a node failure and scales with a one-line command.

![Snapvault running on a GKE Autopilot cluster: a Deployment per service, internal ClusterIP Services for service-to-service calls, a LoadBalancer fronting the gateway, NATS as a StatefulSet, and managed databases outside the cluster.](images/ch09_gke.png)

## Just enough of the Kubernetes object model

Kubernetes is a system for running containers across a pool of machines according to a declared desired state. You don't tell it "start this container on that server"; you tell it "I want three copies of this image running, reachable under this name, restarted if they die," and the cluster's control loop continuously works to make reality match that declaration. Everything is an object you describe in YAML and hand to the API with `kubectl apply`. A handful of object types carry almost all the weight, and each maps onto something we already met in Compose.

A **Pod** is the smallest deployable unit: one or more containers that share a network address and are always scheduled together. In practice each of our services is one container in one Pod. You rarely create Pods directly.

A **Deployment** manages a set of identical Pods. You say "I want N replicas of this image," and the Deployment creates them, watches them, replaces any that crash, and --- crucially --- rolls out new versions by starting new Pods and stopping old ones gradually, so there's no downtime. This is the workhorse object; every stateless Snapvault service is a Deployment. It is the production-grade version of a Compose `service` with a replica count and a restart policy baked in.

A **Service** gives a stable name and address to a set of Pods. Pods are ephemeral --- they come and go, each with a new IP --- so you never talk to a Pod directly. Instead you talk to a Service, which load-balances across whatever Pods currently match its label selector. A `ClusterIP` Service is reachable only inside the cluster (perfect for service-to-service calls like gateway → images); a `LoadBalancer` Service gets a public IP from the cloud (how we'll expose the gateway). This is exactly the DNS-by-service-name we relied on in Compose, made durable.

A **ConfigMap** holds non-secret configuration as key/value pairs, and a **Secret** holds sensitive values like database passwords. Both can be injected into a Pod as environment variables --- which is precisely how every Snapvault service already reads its configuration. Nothing in our code changes; we're just supplying the same `PORT`, `NATS_URL`, and friends from a cluster object instead of a `.env` file.

Finally, **probes** are how Kubernetes knows whether a container is healthy. A **liveness** probe answers "is this process alive, or should I restart it?" A **readiness** probe answers "can this Pod serve traffic right now, or should I keep it out of the Service's rotation?" We built exactly these two endpoints back in Chapter 2 --- `/health/live` and `/health/ready` --- precisely so this chapter would be easy. The foresight pays off now.

## Why Autopilot

GKE offers two modes. In **Standard** mode you manage a pool of virtual machines (nodes): their size, their count, their upgrades. In **Autopilot** mode you don't see nodes at all --- you declare Pods with CPU and memory requests, and Google provisions and bills exactly the capacity those Pods need, patching and scaling the underlying machines for you. For a small team that wants to run services, not babysit servers, Autopilot is the right default: it removes an entire category of operational work, enforces sensible security defaults, and bills per-Pod rather than per-idle-node. We'll use Autopilot for Snapvault. Everything in this chapter except the cluster-creation command is identical on Standard.

::: note
Autopilot requires that every container declares CPU and memory **requests**, and it treats requests as limits unless you say otherwise. That's why every manifest below carries a `resources` block. Far from being a nuisance, this forces the right-sizing discipline you'd want anyway --- we'll return to requests and limits in Chapter 14.
:::

## Creating the cluster

You need the `gcloud` CLI authenticated to your project and the GKE API enabled. Creating an Autopilot cluster is one command:

``` bash
$ gcloud container clusters create-auto snapvault \
    --project="$PROJECT_ID" \
    --region=us-central1
```

This takes a few minutes. When it finishes, point `kubectl` at the new cluster by fetching its credentials:

``` bash
$ gcloud container clusters get-credentials snapvault \
    --region=us-central1 --project="$PROJECT_ID"
$ kubectl get nodes
```

`kubectl` is now talking to your cluster. On Autopilot, `get nodes` may show little or nothing until you schedule a Pod --- nodes appear on demand. Creating a cluster by hand like this is fine for learning, but notice that we've just provisioned real infrastructure with an imperative command nobody can review or reproduce. That tension is the entire subject of Chapter 11; for now, get the cluster running.

Give Snapvault its own namespace so its objects don't mingle with anything else in the cluster:

``` bash
$ kubectl create namespace snapvault
$ kubectl config set-context --current --namespace=snapvault
```

The second command makes `snapvault` the default namespace for the rest of our `kubectl` commands, so we can drop `-n snapvault` everywhere.

Two more pieces must exist before any Pod can start. The first is the Cloud Storage bucket the images and thumbnails services will share; locally that was a Compose volume, but Pods need storage that outlives them. Bucket names are global, so pick your own:

``` bash
$ gcloud storage buckets create gs://snapvault-uploads-prod \
    --project="$PROJECT_ID" --location=us-central1 \
    --uniform-bucket-level-access
```

The second is a Kubernetes **ServiceAccount** for our Pods. Every Deployment below runs as `snapvault`, and a Pod that names a ServiceAccount which doesn't exist is never created. Save `k8s/serviceaccount.yaml`:

``` yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: snapvault
```

For now it's an empty identity. In Chapter 11 we bind it to a Google service account so the Pods can reach the bucket without a key file; until then, uploads to GCS will fail with a permission error, which is a useful reminder of what that binding does.

## Configuration and secrets

Our services read configuration from the environment. In the cluster, the non-secret half lives in a ConfigMap. Create `k8s/config.yaml`:

``` yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: snapvault-config
data:
  NATS_URL: "nats://nats:4222"
  STORAGE: "gcs"
  GCS_BUCKET: "snapvault-uploads-prod"   # the bucket you just created
  MONGO_DB: "snapvault"
  IMAGES_URL: "http://images:8080"
  METADATA_URL: "http://metadata:8080"
  FEED_URL: "http://feed:8080"
  LOG_LEVEL: "info"
```

Those values mirror what we had in Compose, with two changes that matter. `NATS_URL` now points at `nats`, the in-cluster Service we'll create shortly, and the three `*_URL` entries the gateway needs point at the `images`, `metadata`, and `feed` Services --- the same DNS-by-name pattern, just inside Kubernetes. (The gateway refuses to start without `IMAGES_URL` and `FEED_URL`, and Chapter 14's aggregation reads `METADATA_URL`.) `STORAGE` is `gcs` because in a real cluster we use Cloud Storage, not a local disk that vanishes when a Pod is rescheduled.

Connection strings carrying credentials are secrets, not config. Never put a database password in a ConfigMap; ConfigMaps are not encrypted and are readable by anyone with cluster read access. Create a Secret instead. You can do it imperatively so the values never sit in a file in your repo:

``` bash
$ kubectl create secret generic snapvault-secrets \
    --from-literal=MONGO_URI='mongodb+srv://USER:PASS@cluster0.example.mongodb.net' \
    --from-literal=DATABASE_URL='postgres://USER:PASS@DB_HOST:5432/snapvault?sslmode=require'
```

`DB_HOST` is wherever your Postgres is reachable from the cluster. Chapter 11 provisions Cloud SQL and connects to it through the Cloud SQL Auth Proxy, at which point this becomes `127.0.0.1`.

::: warning
A Kubernetes Secret is only **base64-encoded**, not encrypted, in its default form --- it keeps credentials out of your manifests and out of plain sight, but it is not strong protection on its own. GKE encrypts Secret data at rest, and in Chapter 14 we'll graduate to pulling these values from Google Secret Manager via Workload Identity so they never live in the cluster as a Secret at all. For now, a Secret created imperatively is a reasonable starting point.
:::

## A first Deployment and Service: images

Here is the images service as a Deployment plus a Service. Save it as `k8s/images.yaml`:

``` yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: images
  labels:
    app: images
spec:
  replicas: 2
  selector:
    matchLabels:
      app: images
  template:
    metadata:
      labels:
        app: images
    spec:
      serviceAccountName: snapvault
      containers:
        - name: images
          image: us-central1-docker.pkg.dev/PROJECT_ID/snapvault/images:GIT_SHA
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef:
                name: snapvault-config
            - secretRef:
                name: snapvault-secrets
          resources:
            requests:
              cpu: "250m"
              memory: "128Mi"
          livenessProbe:
            httpGet:
              path: /health/live
              port: 8080
            initialDelaySeconds: 3
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /health/ready
              port: 8080
            initialDelaySeconds: 3
            periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: images
spec:
  selector:
    app: images
  ports:
    - port: 8080
      targetPort: 8080
```

There is a lot here, but almost all of it we've already justified. `replicas: 2` runs two copies of the images service; the Deployment keeps both alive and load-balances across them. The `image` is the exact one CI pushed to Artifact Registry in Chapter 8 --- pinned to a commit SHA, never `:latest`, so the cluster runs a known, immutable build. `envFrom` injects the ConfigMap and Secret as environment variables wholesale, which is why our `config.MustString("NATS_URL")` and the rest simply work. The two probes point at the health endpoints we built in Chapter 2: if `/health/live` stops responding the container is restarted; if `/health/ready` returns 503 the Pod is pulled out of the Service until it recovers.

The `Service` object below the Deployment is what makes `http://images:8080` resolve from any other Pod. The gateway will reach the images service through it without knowing or caring how many replicas exist or where they're scheduled. `serviceAccountName: snapvault` attaches the ServiceAccount we created above, which Chapter 11 binds to a Google service account so the Pod can reach Cloud Storage without a key file --- the same Application Default Credentials story from Chapter 5, now wired through workload identity.

Apply it, together with the ServiceAccount and ConfigMap it depends on, and watch the rollout:

``` bash
$ kubectl apply -f k8s/serviceaccount.yaml -f k8s/config.yaml
$ kubectl apply -f k8s/images.yaml
$ kubectl rollout status deployment/images
$ kubectl get pods -l app=images
```

You should see two `images` Pods reach `Running` and then `Ready`. If a Pod crashes, watch Kubernetes start a replacement within seconds --- the self-healing we came here for.

## The worker that has no front door

The thumbnails service is different: in Chapter 6 we deliberately gave it **no HTTP surface**. It's a JetStream consumer that wakes on `images.uploaded`, resizes, and goes back to sleep. That's a clean design, but it creates a small problem for Kubernetes, which prefers an HTTP probe to decide whether a container is alive. A distroless image has no shell, so an `exec` probe is out, and a TCP probe only proves a port is open, not that the consumer is actually processing.

The pragmatic fix is to give even a headless worker a minimal liveness endpoint. We don't add business HTTP --- just enough for a probe. In the thumbnails `main`, alongside starting the consumer, serve `httpx.Health` on a small port in a goroutine:

``` go
// thumbnails/main.go — health-only listener for Kubernetes probes
mux := http.NewServeMux()
ready := func() bool { return atomic.LoadInt32(&consuming) == 1 }
httpx.Health(mux, ready)
go func() {
    addr := ":" + config.String("HEALTH_PORT", "8081")
    if err := http.ListenAndServe(addr, mux); err != nil {
        log.Error("health server stopped", "err", err)
    }
}()
```

`consuming` is an `int32` the worker sets to `1` once `cons.Consume(...)` is established, and back to `0` from a goroutine that waits on the consume context's `Closed()` channel, which closes if consumption stops (say, the durable consumer is deleted). So readiness reflects whether the worker is actually attached to the stream. Now the thumbnails Deployment can carry the same liveness and readiness probes as everything else, pointed at port `8081`. Its manifest looks like the images one but with no `Service` --- nothing calls a worker over the network --- and the probe ports set to `8081`:

``` yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: thumbnails
spec:
  replicas: 1
  selector:
    matchLabels: { app: thumbnails }
  template:
    metadata:
      labels: { app: thumbnails }
    spec:
      serviceAccountName: snapvault
      containers:
        - name: thumbnails
          image: us-central1-docker.pkg.dev/PROJECT_ID/snapvault/thumbnails:GIT_SHA
          envFrom:
            - configMapRef: { name: snapvault-config }
            - secretRef: { name: snapvault-secrets }
          resources:
            requests: { cpu: "250m", memory: "128Mi" }
          livenessProbe:
            httpGet: { path: /health/live, port: 8081 }
          readinessProbe:
            httpGet: { path: /health/ready, port: 8081 }
```

The metadata and feed services follow the images template exactly --- each a Deployment plus a ClusterIP Service on port 8080, each pulling its own image tag, each reading the shared ConfigMap and Secret. Because metadata and feed both run an HTTP read API *and* a JetStream consumer, their readiness can reflect their database connection, which is exactly what we want: a feed Pod that has lost Postgres reports "not ready" and stops receiving gallery requests until it recovers.

## Exposing the gateway

Everything so far is internal. The gateway is the one service the public should reach, so it gets a `LoadBalancer` Service, which provisions a real external IP from Google Cloud. Save `k8s/gateway.yaml`:

``` yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway
spec:
  replicas: 2
  selector:
    matchLabels: { app: gateway }
  template:
    metadata:
      labels: { app: gateway }
    spec:
      containers:
        - name: gateway
          image: us-central1-docker.pkg.dev/PROJECT_ID/snapvault/gateway:GIT_SHA
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef: { name: snapvault-config }
          resources:
            requests: { cpu: "250m", memory: "128Mi" }
          livenessProbe:
            httpGet: { path: /health/live, port: 8080 }
          readinessProbe:
            httpGet: { path: /health/ready, port: 8080 }
---
apiVersion: v1
kind: Service
metadata:
  name: gateway
spec:
  type: LoadBalancer
  selector:
    app: gateway
  ports:
    - port: 80
      targetPort: 8080
```

Apply it, then ask Kubernetes for the assigned address --- it may take a minute to appear while the cloud provisions the load balancer:

``` bash
$ kubectl apply -f k8s/gateway.yaml
$ kubectl get service gateway -w
```

When the `EXTERNAL-IP` changes from `<pending>` to a real address, Snapvault is on the internet. Visiting it hits a gateway Pod, which proxies `/images/` and `/api/...` to the internal Services exactly as it did under Compose.

::: tip
A raw `LoadBalancer` Service is the simplest path and great for getting traffic flowing. For production you'll usually want a **GKE Gateway** or **Ingress** in front instead: it gives you a Google-managed TLS certificate, HTTP-to-HTTPS redirects, and host/path routing, all without terminating TLS in your own code. The gateway service stays exactly the same behind it --- only the Service type and an extra Ingress or Gateway object change. Chapter 14 makes that switch, before the gateway starts accepting bearer tokens.
:::

## Running NATS in the cluster

NATS is stateful --- JetStream persists messages to disk --- so it's a `StatefulSet`, not a Deployment, and it needs a `PersistentVolumeClaim` so its data survives a Pod restart. You can write that YAML by hand, but the maintained path is the official NATS Helm chart, which produces a correct StatefulSet, the headless Service for clustering, and JetStream configuration from a short values file:

``` bash
$ helm repo add nats https://nats-io.github.io/k8s/helm/charts/
$ helm install nats nats/nats --namespace snapvault \
    --set config.jetstream.enabled=true \
    --set config.jetstream.fileStore.pvc.size=10Gi
```

This gives us a `nats` Service on port 4222 --- the address our ConfigMap already points at. A single NATS Pod with JetStream on a persistent volume is enough to run Snapvault; in Chapter 14 we'll turn this into a three-node NATS cluster with replicated streams so the broker itself isn't a single point of failure.

The broker stays a release of its own, named `nats`, for the rest of the book, so `nats://nats:4222` never changes. Chapter 10 shows how a chart *could* bundle it, Chapter 12 hands this release to ArgoCD alongside our services, and Chapter 14 scales it --- always the same release.

## Databases: managed, not in-cluster

It is tempting to run Postgres and MongoDB in the cluster too, as StatefulSets, mirroring our Compose file. Resist it for production. Databases want careful backups, point-in-time recovery, failover, and version upgrades --- exactly the operational burden managed services exist to carry. Run them outside the cluster: **Cloud SQL** for the feed's PostgreSQL, and **MongoDB Atlas** (or self-managed on a VM if you must) for metadata. The services don't care --- they read `DATABASE_URL` and `MONGO_URI` from the Secret, which now point at managed endpoints instead of in-cluster Pods. This is the one place the cluster topology diverges from Compose on purpose, and it's the right divergence. Provisioning that Cloud SQL instance reproducibly is, again, a job for Chapter 11.

## Operating the cluster

With everything applied, the everyday commands are few and worth committing to muscle memory. List what's running and confirm health:

``` bash
$ kubectl get pods
$ kubectl logs deployment/thumbnails
```

Scale the thumbnailer up because uploads are surging:

``` bash
$ kubectl scale deployment/thumbnails --replicas=4
```

Four workers now bind to the same durable `thumbnailer` consumer and JetStream hands each message to exactly one of them --- the competing-consumers pattern from Chapter 6, scaled with one command. Roll out a new version by changing the image tag and applying; the Deployment starts new Pods, waits for them to pass readiness, and only then retires the old ones:

``` bash
$ kubectl set image deployment/images \
    images=us-central1-docker.pkg.dev/PROJECT_ID/snapvault/images:NEW_SHA
$ kubectl rollout status deployment/images
```

If the new version is bad, `kubectl rollout undo deployment/images` returns to the previous one. Kill a Pod by hand --- `kubectl delete pod <name>` --- and watch a replacement appear immediately. This is the payoff: the cluster holds the system to its declared state, heals failures without you, and ships new code without downtime.

### Summary

-   Kubernetes runs containers across many machines toward a **declared desired state**, replacing failed Pods and rolling out new versions without downtime --- the production answer to Compose's single host.
-   A **Deployment** manages replicas of a stateless service; a **Service** gives them a stable name and load-balances across them (`ClusterIP` internal, `LoadBalancer` public).
-   **ConfigMap** and **Secret** supply the same environment variables our services already read; keep credentials in Secrets, never ConfigMaps.
-   Liveness and readiness **probes** point at the `/health/live` and `/health/ready` endpoints we built in Chapter 2 --- that early foresight is why deployment is now mechanical.
-   **GKE Autopilot** removes node management and bills per-Pod, at the cost of requiring CPU/memory **requests** on every container --- a discipline you want anyway.
-   The headless **thumbnails** worker gains a minimal health-only HTTP listener so Kubernetes can probe it, but no `Service`, because nothing calls a worker over the network.
-   Run **NATS** as a StatefulSet (via its Helm chart) with a persistent volume; run **databases as managed services** outside the cluster, not as in-cluster StatefulSets.
-   Day-to-day operation is a handful of `kubectl` commands: `apply`, `rollout status`, `scale`, `set image`, `rollout undo`, `logs`.

We now have Snapvault running on a self-healing cluster. But look at how we got here: five nearly identical Deployment manifests, hand-edited and applied one `kubectl apply -f` at a time, with the image registry and a literal commit SHA baked straight into the YAML. We already felt the alternative when we installed NATS --- a single `helm install` produced a correct StatefulSet, Service, and persistent volume we'd never have wanted to write by hand. The next chapter turns Snapvault's own pile of YAML into that same kind of artifact: a Helm chart that installs, upgrades, and rolls back the whole system as one versioned, parameterized package.
