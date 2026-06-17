# Minimal but exhaustive smoke fixture for the Kilolock HTTP backend.
#
# Goals:
#   * Use providers that need no cloud credentials and no network
#     calls (besides the initial plugin download).
#   * Touch every normalization path the wire contract exposes:
#       - resources at the root
#       - resources inside a module (module_path != "")
#       - an explicit depends_on (resource_dependencies edge)
#       - implicit attribute reference (resource_dependencies edge)
#       - an output (outputs table)
#   * Stay small enough to apply and destroy in <5s on a laptop.
#
# The HTTP backend is intentionally NOT declared here. scripts/smoke.sh
# generates a backend.tf alongside this file at runtime so the same
# fixture can be reused against different server endpoints.

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}

variable "name_length" {
  description = "Length of the random pet name. Bumped between applies to force a replacement."
  type        = number
  default     = 2
}

resource "random_pet" "name" {
  length = var.name_length
}

resource "null_resource" "marker" {
  triggers = {
    pet = random_pet.name.id
  }
}

resource "null_resource" "stamp" {
  triggers = {
    stamp = "kl smoke: ${random_pet.name.id}"
  }
  depends_on = [null_resource.marker]
}

module "tag" {
  source = "./modules/tag"
  pet    = random_pet.name.id
}

output "pet" {
  value = random_pet.name.id
}

output "tag" {
  value = module.tag.tag
}
