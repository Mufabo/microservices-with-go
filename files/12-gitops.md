# Chapter 12 --- GitOps with ArgoCD

We have two halves of a deployment story that don't quite meet. In Chapter 8 the pipeline builds a container image for every service and pushes it to Artifact Registry, tagged by commit --- but it stops there, deliberately, because "an image exists" is not "the image is running," and we wanted deployment to be a separate, conscious act. In Chapter 9 we performed that act by hand: `kubectl apply -f k8s/` to install the manifests, `kubectl set image` to roll out a new version. It worked, but look at what it is --- an imperative command, run from someone's laptop, against the production cluster, leaving no trace anyone can review. It's the exact problem Chapter 11 diagnosed for infrastructure, still sitting unsolved for application deployment. Ask "what is actually running in the cluster right now, and who changed it last?" and the honest answer is "whatever the last person to run `kubectl` did, and you'd have to ask them."

**GitOps** closes that gap. The idea is simple and, once you've lived with it, hard to give up: a Git repository becomes the single source of truth for the desired state of your cluster, and a controller running *inside* the cluster continuously reconciles reality to match what Git says. You don't push changes at the cluster; the cluster pulls them from Git and converges on them. Deploying becomes a commit. Rolling back becomes a revert. "What's running?" becomes "read the repo." This chapter takes Snapvault from hand-run `kubectl` to GitOps with **ArgoCD**, the most widely used GitOps controller for Kubernetes, and in doing so finally connects the image the pipeline built to the workload the cluster runs.

## The principles, and why they matter

GitOps is usually described with four properties, and each one buys something concrete. The desired state is **declarative** --- Kubernetes manifests, not scripts --- so it describes the destination, not the steps. It's **versioned and immutable in Git**, so every change is a commit with an author, a timestamp, a diff, and a review; your deployment history *is* your Git history. It's **pulled automatically** by an in-cluster agent, which means no external system ever needs cluster credentials --- a CI pipeline that can `kubectl apply` to production is a juicy target, and GitOps removes it entirely. And it's **continuously reconciled**: the controller doesn't apply once and forget, it watches forever, so if the live cluster drifts from Git --- someone runs a hotfix `kubectl edit`, a node gets replaced --- the controller notices and corrects it.

Compare this to the Chapter 9 model. There, deployment was push-based: a human (or, worse, a CI job holding cluster credentials) reached into the cluster and mutated it. Nothing recorded the change, nothing detected drift, and rolling back meant remembering the previous image tag and running another imperative command. GitOps inverts all of it. The cluster's state lives in a repo you already know how to review and revert, and the only thing with credentials to change the cluster is a controller that does exactly what the repo tells it.

## What ArgoCD is

ArgoCD is a controller you install into the cluster. Its core abstraction is the **Application** --- a Kubernetes custom resource that maps *a path in a Git repository* to *a destination namespace in a cluster*. You tell an Application "the manifests live at this repo, this branch, this path; deploy them into that namespace," and ArgoCD's components do the rest: a repo-server clones the repo and renders the manifests (running Kustomize or Helm if needed), an application-controller diffs the rendered manifests against what's live and applies the difference, and a web UI and API show you, in real time, whether each Application is **Synced** (cluster matches Git) and **Healthy** (the workloads are actually up). The controller understands Kubernetes health natively --- a Deployment is Healthy once its rollout completes, Degraded if its Pods are crash-looping --- so the dashboard tells you not just "did it apply" but "is it working."

## The config repository

A practical GitOps setup separates two repositories. The **source repository** is what we've been building all along --- Go code, the Dockerfile, the CI workflow on Codeberg. The **config repository** holds the Kubernetes manifests that describe how Snapvault runs: it's the new home for the YAML we wrote in Chapter 9. Keeping them apart matters because they change for different reasons and on different cadences --- application code changes when behavior changes; config changes when a deployment changes (a new image tag, a replica bump, a tweaked resource limit) --- and because it lets the pipeline update the deployed version without touching, or re-triggering, the application repo's own CI.

Create a second Codeberg repository, `snapvault-config`, structured with Kustomize so a shared base can be specialized per environment:

    snapvault-config/
      base/
        serviceaccount.yaml # the ServiceAccount from Chapter 9
        config.yaml         # the ConfigMap
        gateway.yaml        # the Deployments + Services from Chapter 9
        images.yaml
        thumbnails.yaml
        metadata.yaml
        feed.yaml
        networkpolicy.yaml  # who may call images, from Chapter 14
        kustomization.yaml
      overlays/
        prod/
          kustomization.yaml
          hpa.yaml          # the HPAs from Chapter 14
          pdb.yaml          # the PodDisruptionBudget from Chapter 14
          secretproviderclass.yaml  # Secret Manager mounts, from Chapter 14
          ingress.yaml      # HTTPS load balancer + certificate, from Chapter 14

One edit happens on the way into `base/`: the image lines. In Chapter 9 each Deployment named its full registry path and a literal commit SHA. In the base, each names a short placeholder instead --- `image: snapvault/images:latest`, `snapvault/feed:latest`, and so on --- which is environment-neutral and never deployed as-is. The `base/kustomization.yaml` lists the shared resources, and the prod overlay layers environment-specific pieces on top and maps each placeholder to the real image it should run:

``` yaml
# overlays/prod/kustomization.yaml
resources:
  - ../../base
  - hpa.yaml
  - pdb.yaml
namespace: snapvault
images:
  - name: snapvault/gateway            # must match the image name in base
    newName: us-central1-docker.pkg.dev/PROJECT_ID/snapvault/gateway
    newTag: "REPLACED_BY_CI"
  - name: snapvault/images
    newName: us-central1-docker.pkg.dev/PROJECT_ID/snapvault/images
    newTag: "REPLACED_BY_CI"
  # …one entry per service
```

The `name` is how Kustomize finds what to replace: it matches the image name as written in the base manifests, without the tag. If it doesn't match --- say the base still carried Chapter 9's full registry path while the overlay said `name: images` --- Kustomize silently changes nothing, and the cluster tries to pull whatever tag the base literally contains. `kubectl kustomize overlays/prod` renders the overlay locally, and is the quickest way to check that every image came out right. The `newTag` field is the seam where deployment happens: changing it, and committing, is what tells the cluster to run a new version. We'll have CI write it in a moment.

## Installing ArgoCD

ArgoCD installs from a single upstream manifest into a namespace that, by convention, must be called `argocd`:

``` bash
$ kubectl create namespace argocd
$ kubectl apply -n argocd --server-side --force-conflicts \
    -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

The `--server-side --force-conflicts` flags are not optional here: ArgoCD's bundled CRDs (the ApplicationSet one in particular) exceed the size limit that client-side `kubectl apply` can store in an annotation, and a plain apply fails with a `metadata.annotations: Too long` error. For production, pin a released version (for example `v3.2.0`) in that URL instead of `stable`, so an install is reproducible rather than tracking a moving branch.

This being a book about infrastructure as code, though, hand-running even this command should feel slightly wrong --- and it is the one bootstrap exception worth eliminating. The clean approach is to install ArgoCD *from Terraform* (Chapter 11), using the Helm provider, so the controller itself is declared like everything else:

``` hcl
resource "helm_release" "argocd" {
  name             = "argocd"
  namespace        = "argocd"
  create_namespace = true
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = "7.8.0" # pin the chart version
}
```

The `helm_release` resource needs the `hashicorp/helm` provider next to `google` in `providers.tf`, configured to reach the new cluster: its endpoint and CA certificate from `google_container_cluster.snapvault`, and an access token from the `google_client_config` data source. The companion repository's `infra/providers.tf` shows the full block.

This gives a satisfying division of labor: Terraform provisions the cluster and installs exactly one workload into it --- ArgoCD --- and from that point on, *Git drives everything else*. Terraform's job ends where GitOps begins.

## The first Application

Now point ArgoCD at the config repo. An Application is itself a manifest, which you can apply directly or --- better --- keep in Git too. Here is one that deploys the entire prod overlay:

``` yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: snapvault
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://codeberg.org/your-org/snapvault-config.git
    targetRevision: main
    path: overlays/prod
  destination:
    server: https://kubernetes.default.svc   # the cluster ArgoCD runs in
    namespace: snapvault
  syncPolicy:
    automated:
      prune: true       # delete resources removed from Git
      selfHeal: true    # revert manual changes back to Git
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
      - RespectIgnoreDifferences=true   # see the warning below
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers:
        - /spec/replicas
```

Apply that once, and ArgoCD takes over: it clones `snapvault-config`, renders `overlays/prod` with Kustomize, and applies the result into the `snapvault` namespace --- every Deployment and Service from Chapter 9, the ConfigMap, and the HPAs, all from Git. The `automated` sync policy means you never run `kubectl apply` again; a commit to the config repo is the deploy. `prune: true` means deleting a manifest from Git deletes it from the cluster, so Git is *complete* truth, not just additive. `selfHeal: true` means the cluster cannot drift: edit a live Deployment by hand and ArgoCD reverts it within seconds.

One piece of the system isn't in that overlay: NATS, still the `nats` Helm release we installed by hand in Chapter 9. Hand it to ArgoCD too, with a second Application that points straight at the upstream chart --- ArgoCD renders Helm charts natively, so there's nothing to commit but the Application itself:

``` yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: nats
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://nats-io.github.io/k8s/helm/charts/
    chart: nats
    targetRevision: "1.2.*"        # better: pin the exact version you've tested
    helm:
      releaseName: nats            # keeps the Service named `nats`
      valuesObject:
        config:
          jetstream:
            enabled: true
            fileStore:
              pvc: { size: 10Gi }
  destination:
    server: https://kubernetes.default.svc
    namespace: snapvault
  syncPolicy:
    automated: { prune: true, selfHeal: true }
```

The `releaseName` matters: it keeps the Service named `nats`, so `NATS_URL` doesn't change. ArgoCD adopts the existing objects rather than recreating them, and the broker's configuration now lives in Git like everything else. (This is the "direct chart" style from Chapter 10: Helm for third-party software, Kustomize for our own.)

::: warning
The `ignoreDifferences` block is essential the moment you combine GitOps with autoscaling, and leaving it out causes a maddening bug. The HorizontalPodAutoscaler from Chapter 14 owns each Deployment's replica count, scaling it up and down with load. But the manifest in Git also specifies `replicas`, so without intervention ArgoCD sees the HPA's live value differ from Git, "corrects" it back, the HPA scales it again, ArgoCD reverts it again --- the two controllers fight, and your replica count oscillates. Telling ArgoCD to ignore `/spec/replicas` on Deployments hands that field to the HPA and ends the war --- but only with the `RespectIgnoreDifferences=true` sync option next to it. By default `ignoreDifferences` affects only the *diff*: it stops the endless correction loop, but when a sync does happen for another reason, such as CI promoting a new image tag, ArgoCD applies the whole manifest, `replicas: 2` included, and every deploy knocks a scaled-up Deployment back to two Pods. The sync option makes ArgoCD leave ignored fields out of what it applies. Whenever something *other* than Git is the rightful owner of a field, `ignoreDifferences` plus `RespectIgnoreDifferences` is how you say so.
:::

## Many apps from one declaration

A single Application for the whole overlay is fine to start, but real platforms grow to many Applications --- per service, per environment --- and creating each by hand reintroduces exactly the manual toil we're trying to escape. ArgoCD's answer is the **ApplicationSet**, which generates Applications from a template. For Snapvault, the natural use is multiple environments from one definition: a list generator produces a `snapvault-staging` and a `snapvault-prod` Application from a single template, each pointed at its own overlay.

``` yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: snapvault
  namespace: argocd
spec:
  generators:
    - list:
        elements:
          - env: staging
          - env: prod
  template:
    metadata:
      name: "snapvault-{{env}}"
    spec:
      project: default
      source:
        repoURL: https://codeberg.org/your-org/snapvault-config.git
        targetRevision: main
        path: "overlays/{{env}}"
      destination:
        server: https://kubernetes.default.svc
        namespace: "snapvault-{{env}}"
      syncPolicy:
        automated: { prune: true, selfHeal: true }
        syncOptions: [CreateNamespace=true, ServerSideApply=true]
```

Each generated Application expects its own overlay, so this assumes an `overlays/staging/` directory next to `overlays/prod/` --- typically the same base with a smaller bucket, fewer replicas, and its own image tags. With that in place, adding an environment is one overlay directory plus one line in the generator. The same pattern --- sometimes called *App of Apps* when one root Application installs a directory of other Application manifests --- lets the entire platform, including ArgoCD's own configuration, be declared in Git and bootstrapped from a single root the way Terraform created ArgoCD.

## Closing the loop: from a commit to a running version

Here is where Chapter 8's pipeline and this chapter finally meet. The pipeline builds and pushes an image tagged with the commit SHA. To *deploy* that image, something must write the new tag into the config repo's prod overlay; ArgoCD does the rest. We don't give the pipeline cluster credentials --- that's the whole point --- we give it write access to the config repo. Add a job to the Forgejo Actions workflow that runs once after the matrix build and updates every service's tag in a single commit:

``` yaml
  promote:
    needs: build                      # after all images are pushed
    if: github.ref == 'refs/heads/main'
    runs-on: docker
    container:
      image: ghcr.io/catthehacker/ubuntu:act-latest
    steps:
      - name: Install kustomize
        run: |
          curl -sSfL https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh | bash
          mv kustomize /usr/local/bin/

      - name: Update image tags in the config repo
        run: |
          git clone "https://x:${{ secrets.CONFIG_REPO_TOKEN }}@codeberg.org/your-org/snapvault-config.git"
          cd snapvault-config/overlays/prod
          for svc in gateway images thumbnails metadata feed; do
            kustomize edit set image \
              "snapvault/$svc=us-central1-docker.pkg.dev/${{ vars.GCP_PROJECT_ID }}/snapvault/$svc:${{ github.sha }}"
          done
          git -c user.name=ci -c user.email=ci@snapvault commit \
            -am "deploy ${{ github.sha }}"
          git push
```

The left side of each `snapvault/$svc=...` is the placeholder name from the base, the same `name` the overlay's `images:` list matches on. `CONFIG_REPO_TOKEN` is a Codeberg access token scoped to write the config repo --- not a cloud credential, and powerless against the cluster. Running this as one `promote` job rather than inside the five-way build matrix matters: five parallel jobs pushing to the same branch would race and reject each other, so we collect the tags into a single commit. When that commit lands, ArgoCD notices and syncs the new images into the cluster. "Deploy deliberately," promised in Chapter 8, turns out to mean exactly this: a commit to the config repo, reviewable and revertible like any other.

By default ArgoCD polls Git every three minutes, so a deploy can lag by that much. For immediate syncs, add a webhook in the Codeberg repository pointing at ArgoCD's `/api/webhook` endpoint; a push then triggers a sync at once. (If the config repo is private, ArgoCD needs read credentials too, supplied as a repository Secret in the `argocd` namespace.)

::: tip
There are two ways to get a new image's tag into Git, and it's worth knowing both. The **CI-writes-the-tag** approach above is explicit and keeps the pipeline in charge of promotion, which makes gating between environments (deploy to staging always, to prod only on a release tag) a matter of ordinary workflow logic. The alternative is **Argo CD Image Updater**, a companion controller that watches your registry directly and writes new tags into the config repo itself, no CI step required. Image Updater is convenient for "always run the newest build" flows; explicit CI promotion is usually clearer when you want deployment to be a deliberate, gated decision --- which, for Snapvault, we do.
:::

## Drift, rollback, and ordering

Three everyday behaviors show why GitOps changes how it *feels* to operate a system. **Drift** simply can't persist: with `selfHeal` on, a well-meaning `kubectl scale` or `kubectl edit` against the live cluster is reverted to match Git within seconds, so the cluster is always exactly what the repo says --- there is no such thing as an undocumented production change anymore. **Rollback** is `git revert`: to undo a bad deploy, revert the commit that changed the image tag and let ArgoCD sync the previous version back. (One sharp edge to know: ArgoCD refuses `argocd app rollback` while automated sync is enabled, because auto-sync would immediately re-apply Git; reverting in Git is the GitOps-native way, and it keeps the rollback itself in your history.) And **ordering** --- for cases where one resource must exist before another, like the database schema being migrated before new code starts --- ArgoCD offers sync waves and resource hooks: package the Chapter 8 migration as a Kubernetes Job (the `migrate` image running against the `migrations/` directory, connecting through the same Cloud SQL Auth Proxy as the feed), annotate it as a `PreSync` hook, and it runs to completion before the Deployments roll, giving you the "migrate, then deploy" sequencing as a property of the manifests rather than a step in a script.

Step back and notice the shift. Deployment is no longer an action a person takes against the cluster; it's a state the cluster maintains by reading Git. Every change is reviewed, recorded, and reversible; the cluster cannot drift; and nothing outside the cluster holds the keys to change it. The imperative `kubectl` of Chapter 9 was the right way to *learn* Kubernetes; GitOps is the right way to *run* it.

### Summary

-   **GitOps** makes a Git repo the single source of truth for cluster state and runs an in-cluster controller that continuously reconciles the live cluster to match it --- pull-based, not push-based.
-   It buys four things at once: a reviewed, audited history (deploys are commits), trivial rollback (`git revert`), no cluster credentials in CI, and automatic drift correction.
-   **ArgoCD**'s core object is the **Application**, mapping a Git repo + path to a destination namespace; it renders Kustomize/Helm, applies the diff, and reports **Synced** and **Healthy** status natively.
-   Keep a separate **config repository** for manifests (the Chapter 9 YAML), structured with Kustomize as `base` plus per-environment `overlays`, so deployment changes don't churn the application repo. Base manifests use placeholder image names (`snapvault/<svc>`) that the overlay's `images:` list maps to real, tagged images.
-   Third-party software like **NATS** gets its own Application pointing directly at its Helm chart, so the broker is under GitOps too.
-   Install ArgoCD with `kubectl apply --server-side --force-conflicts` (the CRDs are too large for client-side apply); better, **bootstrap it from Terraform's Helm provider** so Terraform's only workload is ArgoCD and Git drives the rest.
-   An Application with `automated` sync (`prune` + `selfHeal`) means a commit is the deploy and the cluster can't drift; **`ignoreDifferences` on `/spec/replicas`**, with the `RespectIgnoreDifferences=true` sync option, is mandatory so ArgoCD doesn't fight the HPA from Chapter 14.
-   Scale to many apps and environments with an **ApplicationSet** (or App of Apps) generated from one template.
-   Close the loop from Chapter 8 by having CI write the new image tag into the config repo (`kustomize edit set image`) in a single **promote** job --- deploying deliberately, with no cluster credentials, just write access to a repo.

Snapvault now deploys itself: the pipeline builds an image, a commit promotes it, and ArgoCD makes the cluster match. But a system that deploys continuously also needs to be *seen* continuously --- when a sync goes out, is the new version actually healthy, fast, and serving traffic? The next chapter gives Snapvault eyes: logs, metrics, and traces, flowing into Google Cloud's observability stack.
