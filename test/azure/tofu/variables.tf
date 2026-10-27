# All set by test/azure/tofu.sh from .env.azure (TF_VAR_*); none has a
# default that names an account, so a bare `tofu plan` in this directory
# asks rather than guesses.

variable "subscription_id" {
  description = "ICBFS_TEST_AZURE_SUBSCRIPTION: the subscription the tests' account lives in."
  type        = string
}

variable "resource_group" {
  description = "ICBFS_TEST_AZURE_RESOURCE_GROUP: a resource group of the tests' own, created here."
  type        = string
}

variable "location" {
  description = "ICBFS_TEST_AZURE_LOCATION"
  type        = string
}

variable "container" {
  description = "ICBFS_TEST_AZURE_CONTAINER"
  type        = string
}

variable "shared_keys" {
  description = "Whether the account accepts its shared key. On until the store can open with a token (ROADMAP N1)."
  type        = bool
  default     = true
}

variable "readers_group" {
  description = "Tenant security group given Storage Blob Data Reader on the account."
  type        = string
  default     = "icbfs-test-readers"
}

variable "writers_group" {
  description = "Tenant security group given Storage Blob Data Contributor on the account."
  type        = string
  default     = "icbfs-test-writers"
}
