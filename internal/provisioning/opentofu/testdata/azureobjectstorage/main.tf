terraform {
  required_version = "= 1.12.6"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "= 4.46.0"
    }
  }
}

provider "azurerm" {
  features {}
}

variable "liftr" {
  type = object({
    resourceId       = string
    operationId      = string
    attemptNumber    = number
    targetGeneration = number
    capability       = string
    desiredPresent   = bool
  })
}

variable "platform" {
  type = object({
    identity        = string
    location        = string
    replicationType = string
  })
}

variable "spec" {
  type = object({
    accessFrequency       = string
    permitAnonymousAccess = bool
  })

  validation {
    condition     = contains(["frequent", "infrequent"], var.spec.accessFrequency)
    error_message = "accessFrequency must be frequent or infrequent."
  }
}

resource "terraform_data" "liftr_control" {
  input = var.liftr
}

locals {
  storage_name = substr("liftr${sha256("${var.platform.identity}:${var.liftr.resourceId}")}", 0, 24)
  access_tier  = var.spec.accessFrequency == "frequent" ? "Hot" : "Cool"
}

resource "azurerm_resource_group" "storage" {
  count    = var.liftr.desiredPresent ? 1 : 0
  name     = "liftr-${substr(sha256("${var.platform.identity}:${var.liftr.resourceId}"), 0, 16)}-rg"
  location = var.platform.location

  tags = {
    "liftr.io/managed" = "true"
  }
}

resource "azurerm_storage_account" "storage" {
  count                    = var.liftr.desiredPresent ? 1 : 0
  name                     = local.storage_name
  resource_group_name      = azurerm_resource_group.storage[0].name
  location                 = azurerm_resource_group.storage[0].location
  account_tier             = "Standard"
  account_replication_type = var.platform.replicationType
  account_kind             = "StorageV2"
  access_tier              = local.access_tier

  https_traffic_only_enabled       = true
  min_tls_version                  = "TLS1_2"
  allow_nested_items_to_be_public  = var.spec.permitAnonymousAccess
  shared_access_key_enabled        = false
  default_to_oauth_authentication  = true
  cross_tenant_replication_enabled = false
  public_network_access_enabled    = true

  tags = {
    "liftr.io/managed" = "true"
  }
}

output "liftr_envelope" {
  value = {
    version          = 1
    mapping          = "liftr-azure-object-storage-outputs-v1"
    resourceId       = var.liftr.resourceId
    targetGeneration = var.liftr.targetGeneration
    values = {
      endpoint = var.liftr.desiredPresent ? azurerm_storage_account.storage[0].primary_blob_endpoint : "absent"
    }
  }
}
