variable "project_id" {
  type        = string
  description = "Google Cloud project ID."
}

variable "region" {
  type        = string
  description = "Region for regional resources."
  default     = "europe-west1"
}

variable "forgejo_repo" {
  type        = string
  description = "owner/repo on Codeberg permitted to deploy (e.g. example/snapvault)."
}

variable "forgejo_issuer" {
  type        = string
  description = "OIDC issuer of the Forgejo instance."
  default     = "https://codeberg.org/api/actions"
}

variable "auth_authorized_domains" {
  type        = list(string)
  description = "Web origins allowed to sign users in with Identity Platform (your app's domain, plus localhost for development)."
  default     = ["localhost"]
}
