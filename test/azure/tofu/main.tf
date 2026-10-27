# The Azure storage account the tests write to (test/azure/run-conformance.sh,
# the multi-machine tests with ICBFS_MULTI_STORE=azure, and the Entra ID
# work of ROADMAP Part N). Run through test/azure/tofu.sh, which sources
# .env.azure for the names; state stays local and out of git.
#
# One account, one container, blob versioning on, no soft delete (the
# tests delete versions and expect them gone), a lifecycle rule that
# expires anything the tests leave after two days, and the roles that
# Part N's sign-in modes need: whoever applies this gets Storage Blob Data
# Contributor, and the tenant's icbfs-test-readers and icbfs-test-writers
# groups get Reader and Contributor on the account.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
    azuread = {
      source  = "hashicorp/azuread"
      version = "~> 3.0"
    }
  }
}

provider "azurerm" {
  subscription_id = var.subscription_id
  use_cli         = true
  features {}
}

provider "azuread" {
  use_cli = true
}

data "azurerm_client_config" "current" {}

data "azuread_group" "readers" {
  display_name     = var.readers_group
  security_enabled = true
}

data "azuread_group" "writers" {
  display_name     = var.writers_group
  security_enabled = true
}

resource "azurerm_resource_group" "test" {
  name     = var.resource_group
  location = var.location
  tags     = local.tags
}

locals {
  # Storage account names are 3 to 24 lowercase letters and digits, unique
  # across all of Azure. The subscription id's first six hex digits make
  # this one both unique enough and the same on every apply.
  account_name = "icbfstest${substr(replace(var.subscription_id, "-", ""), 0, 6)}"

  tags = {
    purpose = "icbfs tests"
    managed = "icbfs/test/azure/tofu"
  }
}

resource "azurerm_storage_account" "test" {
  name                     = local.account_name
  resource_group_name      = azurerm_resource_group.test.name
  location                 = azurerm_resource_group.test.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  access_tier              = "Hot"

  # The key stays on until Part N1 lands and the store can open with a
  # token; then shared_keys = false here is the conformance run's proof.
  shared_access_key_enabled       = var.shared_keys
  default_to_oauth_authentication = true
  allow_nested_items_to_be_public = false

  https_traffic_only_enabled = true
  min_tls_version            = "TLS1_2"

  blob_properties {
    versioning_enabled = true
  }

  tags = local.tags
}

resource "azurerm_storage_container" "test" {
  name                  = var.container
  storage_account_id    = azurerm_storage_account.test.id
  container_access_type = "private"
}

# Each test writes under a prefix of its own and cleans up after itself;
# this is for what a killed run leaves behind.
resource "azurerm_storage_management_policy" "expire" {
  storage_account_id = azurerm_storage_account.test.id

  rule {
    name    = "expire-test-data"
    enabled = true

    filters {
      blob_types = ["blockBlob"]
    }

    actions {
      base_blob {
        delete_after_days_since_modification_greater_than = 2
      }
      version {
        delete_after_days_since_creation = 2
      }
    }
  }
}

# Roles on the account. Entra roles and these are separate hierarchies: an
# Owner of the subscription still cannot read a blob without one of these
# once shared keys are off.
#
# Deleting a blob *version* (icbfs's Erase: purging the trash, dropping a
# version for good) is its own action, deleteBlobVersion, which Storage
# Blob Data Contributor lacks and Owner has; Azure refuses it with the
# code OperationNotAllowedOnAutomaticSnapshot, not a 403. The conformance
# suite erases, so whoever runs it is Owner here. Writers in production
# can write and delete but not erase unless given the same.
resource "azurerm_role_assignment" "deployer" {
  scope                = azurerm_storage_account.test.id
  role_definition_name = "Storage Blob Data Owner"
  principal_id         = data.azurerm_client_config.current.object_id
}

resource "azurerm_role_assignment" "readers" {
  scope                = azurerm_storage_account.test.id
  role_definition_name = "Storage Blob Data Reader"
  principal_id         = data.azuread_group.readers.object_id
  principal_type       = "Group"
}

resource "azurerm_role_assignment" "writers" {
  scope                = azurerm_storage_account.test.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = data.azuread_group.writers.object_id
  principal_type       = "Group"
}
