# Google Identity Platform signs Snapvault's users in and issues the OIDC ID
# tokens the gateway verifies. (Not Cloud Identity, which manages an
# organization's own staff.) The gateway needs only the issuer and audience
# below; the app's sign-in UI uses the Firebase Auth SDK against this project.

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

# "Sign in with Google" needs an OAuth client, which is created in the console
# (APIs & Services > Credentials); add it with
# google_identity_platform_default_supported_idp_config (idp_id "google.com").
