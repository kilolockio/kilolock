# A trivial child module so the smoke fixture exercises module_path
# normalization (canonical addresses like module.tag.random_id.this).

terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "pet" {
  type = string
}

resource "random_id" "this" {
  byte_length = 4
  keepers = {
    pet = var.pet
  }
}

output "tag" {
  value = "${var.pet}-${random_id.this.hex}"
}
