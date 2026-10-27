output "account" {
  value = azurerm_storage_account.test.name
}

output "container" {
  value = azurerm_storage_container.test.name
}

output "resource_group" {
  value = azurerm_resource_group.test.name
}

# The lines .env.azure wants, for `tofu.sh env`.
output "env" {
  value = <<-EOT
    ICBFS_TEST_AZURE_SUBSCRIPTION=${var.subscription_id}
    ICBFS_TEST_AZURE_RESOURCE_GROUP=${azurerm_resource_group.test.name}
    ICBFS_TEST_AZURE_LOCATION=${azurerm_resource_group.test.location}
    ICBFS_TEST_AZURE_ACCOUNT=${azurerm_storage_account.test.name}
    ICBFS_TEST_AZURE_CONTAINER=${azurerm_storage_container.test.name}
  EOT
}
