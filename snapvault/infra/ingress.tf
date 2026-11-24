# A stable public IP for the gateway's HTTPS load balancer. The prod overlay's
# Ingress claims it by name (kubernetes.io/ingress.global-static-ip-name), and
# your domain's DNS A record points at it, so the address survives the Ingress
# being recreated.
resource "google_compute_global_address" "gateway" {
  name = "snapvault-gateway-ip"
}
