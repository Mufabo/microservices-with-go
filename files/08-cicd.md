# Chapter 8 --- Continuous Integration and Delivery

Tests that only run when someone remembers to run them are a suggestion, not a safety net. The same is true of builds: a release process that lives in one engineer's shell history is a single point of failure with a pulse. Continuous integration and delivery (CI/CD) turns both into automation that runs on every push --- test the code, and if it passes, build a container image for each service and publish it to a registry, tagged so you always know exactly what's running. In this chapter we build that pipeline with **Forgejo Actions** --- the CI/CD system built into Forgejo, the software that powers Codeberg --- authenticate to Google Cloud without any long-lived keys using **Workload Identity Federation**, and push images to **Artifact Registry**. The result is the bridge between "it works on my laptop" and "it's deployable to a cluster."

## What the pipeline does

A good pipeline for a system like Snapvault has two phases with a hard gate between them. First, **integrate**: check out the code, run `go vet` and the unit tests with the race detector --- plus, where you have a runner with Docker, the Testcontainers integration tests from Chapter 7. If anything fails, stop --- nothing downstream runs. Second, **deliver**: for each of the five services, build its container image with the single parameterized Dockerfile from Chapter 3, and push it to Artifact Registry tagged with the commit SHA. Crucially, *delivery* here means "publish a deployable artifact," not "deploy to production." Whether a published image gets rolled out to the cluster is a separate decision (often a manual approval, or a push to a release branch); conflating "built" with "live" is how teams ship Friday-afternoon surprises. We publish on every push to `main`; we deploy deliberately --- and in Chapter 12 we make that deliberate deployment a GitOps commit, so "deploy" stops being a `kubectl` command and becomes a reviewed change in Git.

The two-phase shape --- test gate, then a fan-out build --- maps naturally onto our architecture. Because every service builds from the *same* Dockerfile with only the `SERVICE` build argument changing, the build phase is a single job run five times in parallel over a matrix, not five hand-maintained build scripts. The uniformity we built into the containers in Chapter 3 pays off again here.

## The integration job

A Forgejo Actions workflow is a YAML file in `.forgejo/workflows/`. Start with the test gate. Create `.forgejo/workflows/ci.yaml`:

``` yaml
name: ci

on:
  push:
    branches: [main]
    tags: ["v*"]
  pull_request:

jobs:
  test:
    runs-on: docker
    container:
      image: ghcr.io/catthehacker/ubuntu:act-latest
    steps:
      - uses: https://code.forgejo.org/actions/checkout@v4

      - uses: https://code.forgejo.org/actions/setup-go@v5
        with:
          go-version: "1.26"
          cache: true

      - name: Vet
        run: go vet ./...

      - name: Test (unit, with race detector)
        run: go test -race ./...
```

That is a complete, useful CI in fewer lines than the explanation. It runs only the unit tests, and that's deliberate: as the warning below explains, the integration tests need a Docker daemon that Codeberg's hosted runners don't provide. It runs on every pull request (so you never merge red code) and on every push to `main` and version tags. A few things differ from a GitHub-style workflow and are worth naming, because they're the differences you'll hit when adapting any existing workflow to Forgejo.

`runs-on: docker` selects a runner by **label**, not by a fixed OS image. A Forgejo Runner advertises labels, and jobs are dispatched to a runner offering the requested one; `docker` is the conventional label for "run this job in a container." The `container.image` then pins exactly which image the steps run inside --- here `ghcr.io/catthehacker/ubuntu:act-latest`, a GitHub-runner-like image that Codeberg's hosted runner uses by default and that carries the tools most workflows expect. And actions are referenced by **fully qualified URL** (`https://code.forgejo.org/actions/checkout@v4`) rather than the bare `actions/checkout@v4` you'd write on GitHub. You *can* use the short form, but the prefix it expands to (`DEFAULT_ACTIONS_URL`) is set per instance, so the full URL is unambiguous and portable. Forgejo maintains mirrors of the common actions at `code.forgejo.org/actions/`. The `cache: true` on `setup-go` caches the module and build cache between runs, turning a cold two-minute build into a warm twenty-second one.

::: warning
**Integration tests need a Docker daemon, and that shapes where they run.** The `-tags=integration` tests spin up real Postgres and NATS containers via Testcontainers, which requires access to a Docker daemon --- and so does the `docker build` in the delivery phase below. Codeberg's *hosted* runners deliberately do **not** offer Docker-in-Docker, so these jobs won't work on them as written. The clean answer is to register your own **self-hosted Forgejo Runner** with the host's Docker socket available, and target it with a label (for example `runs-on: [self-hosted, docker]`). A self-hosted runner connects *outbound* to Codeberg and waits for jobs, so it needs no public address --- it can run on a small VM or even a machine at home. This also neatly keeps the jobs that hold your cloud credentials on infrastructure you control. Pure unit tests (`go test ./...` with no tag) have no such requirement and run fine on a hosted runner.
:::

Once you have such a runner, add the integration tests as a job of their own, and make the build depend on it as well as on `test`:

``` yaml
  integration:
    runs-on: [self-hosted, docker]   # your runner, with the Docker socket
    steps:
      - uses: https://code.forgejo.org/actions/checkout@v4
      - uses: https://code.forgejo.org/actions/setup-go@v5
        with:
          go-version: "1.26"
      - name: Integration tests (real Postgres and NATS via Testcontainers)
        run: go test -race -tags=integration ./...
```

Snapvault's companion repository ships without this job, because a job that targets a label no runner offers simply waits in the queue forever. Add it when you register the runner.

::: tip
**Pin actions, watch your supply chain.** `code.forgejo.org/actions/checkout@v4` pins to a major version, which is the common convention, but the most security-conscious teams pin to a full commit SHA so a compromised or retagged action can't silently change what runs in their pipeline. Your CI has credentials to your cloud; treat the actions it runs as dependencies you vet, not magic you trust blindly.
:::

## Authenticating to Google Cloud without keys

The delivery phase needs to push images to Artifact Registry, which means the workflow must authenticate to Google Cloud. The old way to do this was to create a service-account key --- a JSON file with long-lived credentials --- and paste it into a CI secret. Don't. A static key in CI is a credential that can't be rotated easily, leaks through logs and forks, and grants standing access to your cloud to anyone who obtains it. It is one of the most common cloud-breach vectors there is.

The modern way is **Workload Identity Federation** (WIF), which establishes *keyless* trust: you tell Google Cloud to trust short-lived OpenID Connect (OIDC) identity tokens that your Forgejo instance mints for a specific repository, and to let them act as a designated service account. No secret is stored anywhere; Forgejo proves the run's identity cryptographically, and Google issues a credential that lives for minutes. We'll provision the WIF pool and the service account with Terraform in Chapter 11 (it's a one-time setup); here is how the pipeline *uses* it:

``` yaml
  build:
    needs: test                    # only run if the test job passed
    if: github.event_name == 'push'
    runs-on: docker
    container:
      image: ghcr.io/catthehacker/ubuntu:act-latest
    enable-openid-connect: true    # REQUIRED: mint an OIDC token for keyless auth
    strategy:
      matrix:
        service: [gateway, images, thumbnails, metadata, feed]
    steps:
      - uses: https://code.forgejo.org/actions/checkout@v4

      - id: auth
        uses: https://github.com/google-github-actions/auth@v2
        with:
          project_id: ${{ vars.GCP_PROJECT_ID }}
          workload_identity_provider: ${{ vars.WIF_PROVIDER }}
          service_account: ${{ vars.DEPLOY_SA }}

      - uses: https://github.com/google-github-actions/setup-gcloud@v2

      - name: Configure Docker for Artifact Registry
        run: gcloud auth configure-docker us-central1-docker.pkg.dev --quiet

      - name: Build image
        run: |
          docker build \
            --build-arg SERVICE=${{ matrix.service }} \
            -t snapvault-build .

      - name: Tag and push by commit SHA
        run: |
          REPO="us-central1-docker.pkg.dev/${{ vars.GCP_PROJECT_ID }}/snapvault/${{ matrix.service }}"
          docker tag snapvault-build "$REPO:${{ github.sha }}"
          docker push "$REPO:${{ github.sha }}"

      - name: Also tag with the release version
        if: startsWith(github.ref, 'refs/tags/v')
        run: |
          REPO="us-central1-docker.pkg.dev/${{ vars.GCP_PROJECT_ID }}/snapvault/${{ matrix.service }}"
          docker tag snapvault-build "$REPO:${{ github.ref_name }}"
          docker push "$REPO:${{ github.ref_name }}"
```

The line that makes WIF work is `enable-openid-connect: true`. With it set on the job, Forgejo injects `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN` into the run, and the `auth` action uses them to fetch an OIDC token and exchange it with Google for short-lived credentials. Without it, the job can't mint a token and auth fails with a confusing error. The three `vars` passed to the action are *configuration variables*, not secrets --- and that's the point: the provider name and the service-account email are not sensitive, because possessing them grants nothing. Only a token from the trusted repository, verified by Google, unlocks access. There is no key to leak.

::: note
**Forgejo's context, and the Google actions, are the GitHub ecosystem reused.** Two things in that workflow look like GitHub and are deliberately so. The `${{ github.sha }}`, `${{ github.ref }}`, and `${{ github.event_name }}` expressions read Forgejo's **GitHub-compatible context** --- Forgejo populates the same `github.*` object precisely so this ecosystem of expressions and actions works with minimal change. And `google-github-actions/auth` is Google's own action, referenced here by its full GitHub URL because it isn't mirrored on Codeberg; Forgejo can fetch and run actions from arbitrary Git URLs, GitHub included. You're using the broad GitHub Actions ecosystem from a non-GitHub forge --- which is exactly Forgejo Actions' design goal.
:::

## Tagging: the commit SHA is the source of truth

Notice the tagging discipline, promised back in Chapter 3 and delivered here. Every build is tagged with `${{ github.sha }}` --- the exact Git commit it was built from. This is the tag deployments reference, because it answers the two questions that matter in an incident precisely: "what is running in production?" (a specific commit) and "what changed?" (the diff between two commits). On a release tag like `v1.2.0`, we *additionally* tag the image with that version for human-friendly reference, but the SHA is always present and always immutable. We never push a bare `:latest` that deployments depend on, because `:latest` is a moving target that makes "roll back to what we had five minutes ago" a guessing game.

::: note
**One build, many tags.** The workflow builds the image *once* (`snapvault-build`) and then applies multiple tags to that single built image before pushing. This matters: tagging is just attaching names to the same content-addressed layers, so the SHA tag and the version tag refer to *byte-identical* images. Building twice --- once per tag --- would risk producing two subtly different images for the same commit, which defeats the purpose of tagging by commit at all.
:::

## Schema migrations belong in the pipeline too

In Chapter 5 the feed service created its table with `CREATE TABLE IF NOT EXISTS` on startup --- fine for a book, risky in production, where multiple replicas starting at once shouldn't race to alter a schema, and where you need a reviewable history of how the schema evolved. The production-grade approach is *versioned migrations*: numbered SQL files (`001_init.sql`, `002_add_tags.sql`, ...) applied in order by a tool like `golang-migrate`, with the database recording which migrations have run.

Migrations then become a deliberate step that runs *before* the new code is deployed, so the schema is always ready for the code that depends on it. In a pipeline it looks like this:

``` yaml
      - name: Apply database migrations
        run: migrate -path ./migrations -database "$DATABASE_URL" up
```

That one line hides two prerequisites: the runner needs the `migrate` binary, and it needs a network path to the database --- which, for the Cloud SQL instance of Chapter 11, means running the Cloud SQL Auth Proxy in the job. Snapvault keeps its schema in `migrations/001_init.sql` but, being a single additive table, still relies on the feed service's idempotent startup `CREATE TABLE IF NOT EXISTS`; we haven't wired a migration step into its pipeline. Chapter 12 shows the alternative that avoids giving CI database access at all: running the migration as a Kubernetes Job inside the cluster, before the new Pods roll out.

The discipline that keeps this safe is to make migrations *backward compatible* with the currently running version --- add a nullable column, don't rename one out from under live pods --- so that during a rolling deploy, old and new code can both run against the same schema for the minutes it takes to roll over. This is the same eventual-consistency mindset from Chapter 6, applied to schema: change in small, compatible steps rather than big-bang cutovers.

## A note on runners, and on Woodpecker

Two choices are worth knowing about, because they change where your CI compute lives. The first is the runner itself. A Forgejo Runner can be **hosted by Codeberg** (simplest, but with resource and time limits and no Docker-in-Docker) or **self-hosted** (you run `forgejo-runner` on your own machine, it connects outbound to Codeberg, and you decide what it can access --- including the Docker socket the integration and build jobs need). Because a self-hosted runner can run anywhere, a tidy production setup is to run it *inside Google Cloud* --- on a small GCE VM or in the GKE cluster itself --- so the jobs that build and push images sit right next to the registry and cluster they target, while your source still lives on Codeberg.

The second is that Codeberg offers a *second*, independent CI system alongside Forgejo Actions: **Woodpecker CI**. Woodpecker uses its own `.woodpecker.yml` pipeline format rather than the Actions YAML, and Codeberg hosts it for you with an approval workflow, which is why Codeberg currently recommends it when you want hosted CI without running your own runner. The trade-off is the usual one: Forgejo Actions reuses the large GitHub-Actions ecosystem of prebuilt actions and lives in the same Forgejo UI as your code, while Woodpecker is a mature, hosted-for-you option with a smaller, simpler model. Both are good; this book stays with Forgejo Actions because the ecosystem compatibility --- including the Google auth action and WIF flow above --- does the most work for us.

## The pipeline as a contract

Step back and notice what the pipeline now guarantees. Every change that reaches `main` has passed vet and the unit tests under the race detector --- and, once you add the self-hosted integration job, the tests against a real database and a real broker too. Every service has a deployable image in Artifact Registry, tagged with the exact commit it came from, built reproducibly from one reviewed Dockerfile. Nobody builds images by hand, nobody pushes from a laptop, and nobody can deploy a commit that didn't pass tests. That set of guarantees is what lets a team move quickly *without* moving recklessly --- the entire promise of continuous delivery, and the precondition for the cluster work that comes next.

### Summary

-   CI/CD runs on every push: a **test gate** (vet plus unit tests, and integration tests on a runner with Docker) that must pass before a **delivery** phase builds and publishes images.
-   "Delivered" means a deployable image exists in the registry --- not that it's live; deployment to production stays a separate, deliberate decision.
-   **Forgejo Actions** workflows live in `.forgejo/workflows/`, select runners by **label** (`runs-on: docker`) with a pinned `container.image`, and reference actions by **full URL** --- but otherwise reuse the GitHub Actions ecosystem and `github.*` context.
-   Integration tests and image builds need a **Docker daemon**, so run them on a **self-hosted runner** with the Docker socket; Codeberg's hosted runners don't offer Docker-in-Docker.
-   The single parameterized Dockerfile lets the build phase be one matrix job over all five services, run in parallel, instead of five maintained scripts.
-   Authenticate CI to Google Cloud with **Workload Identity Federation** --- keyless, short-lived, repository-scoped trust --- enabled per job with `enable-openid-connect: true`; never a long-lived service-account key in a secret.
-   Tag every image with the **commit SHA** (immutable, the source of truth for "what's running"), add a semver tag on releases, and never let deployments depend on a bare `:latest`. Build once, apply many tags.
-   Run schema **migrations** as a versioned, backward-compatible step before deploying the code that needs them --- in the pipeline (which then needs database access) or as an in-cluster Job (Chapter 12).
-   Codeberg also offers **Woodpecker CI** as a hosted alternative; the runner can be Codeberg-hosted or self-hosted (including inside Google Cloud).

We have images in a registry, built and verified automatically. Now we need somewhere to run them at scale, with self-healing and rolling updates. That's Kubernetes, and Chapter 9 takes Snapvault to a real cluster on GKE.
