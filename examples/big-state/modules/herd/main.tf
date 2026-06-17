# A child module that produces 2 * var.size resources plus one
# "leader" resource. Each indexed instance depends on the leader, so
# the resulting graph has 2 * var.size + 1 resources and 2 * var.size
# dependency edges -- linear in size, not quadratic.
#
# Linearity matters: an earlier draft of this module had each
# random_string instance reference random_id.tag[count.index], which
# terraform records as a *module-scoped* dependency. The normalizer
# then expanded that single reference to every counted sibling,
# producing var.size * var.size edges. At size=50000 that means 2.5
# billion edges -- not a useful demo. The leader pattern below keeps
# the graph realistic-shaped without that footgun.

terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "size" {
  description = "Number of (tag, label) pairs to create. Total resource count is 2 * size + 1."
  type        = number
}

variable "prefix" {
  description = "Logical prefix; baked into the leader so child modules are distinguishable in state."
  type        = string
}

variable "label_length" {
  description = "Length of the random_string payload per entry. Bigger values inflate state file size."
  type        = number
  default     = 32
}

# Single root resource for this module. Every indexed instance below
# depends on it through a scalar reference, which terraform records as
# a precise (non-fan-out) edge in the dependencies array.
resource "random_id" "leader" {
  byte_length = 16

  keepers = {
    prefix = var.prefix
  }
}

resource "random_id" "tag" {
  count       = var.size
  byte_length = 8

  keepers = {
    leader = random_id.leader.hex
  }
}

resource "random_string" "label" {
  count   = var.size
  length  = var.label_length
  special = false
  upper   = false

  keepers = {
    leader = random_id.leader.hex
  }
}

output "leader" {
  description = "The leader hex, which seeds every tag and label in this herd."
  value       = random_id.leader.hex
}

output "size" {
  description = "How many (tag, label) pairs were produced (echoes the input)."
  value       = var.size
}
