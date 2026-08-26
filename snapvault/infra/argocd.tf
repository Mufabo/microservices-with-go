# Install ArgoCD via Helm so the cluster can pull deployments from the config
# repo. After apply, point ArgoCD at the repo with
# deploy/k8s/argocd-application.yaml.
resource "helm_release" "argocd" {
  name             = "argocd"
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = "7.7.0"
  namespace        = "argocd"
  create_namespace = true

  depends_on = [google_container_cluster.autopilot]
}
