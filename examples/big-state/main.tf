# examples/big-state — Kilolock demo at scale.
#
# Produces 2 * var.size + 6 managed resources with roughly 2 * var.size + 5
# dependency edges and 4 outputs. With the default size=50000 that's
# 100,006 resources and ~100,005 edges, large enough to make the contrast
# with a flat .tfstate file obvious without needing any cloud credentials.
# Resource and edge counts are linear in size (not quadratic) -- see the
# header comment in modules/herd/main.tf for the rationale.
#
# See README.md in this directory for run instructions and example
# queries. backend.tf next to this file points the http backend at a
# locally-running `kl serve`.

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
    # time provider gives us `time_sleep`, the simulated-slow-resource
    # primitive used by the v2d parallel-apply demo (slow_a / slow_b
    # below). It has no remote calls — the "delay" is a local goroutine
    # sleep inside the provider — so the demo doesn't need any cloud
    # credentials to feel realistic.
    time = {
      source  = "hashicorp/time"
      version = "~> 0.11"
    }
  }
}

variable "size" {
  description = "Pair count for each herd module. Total resources = 2 * size + 4. Default produces 100k+ resources; override with -var=size=100 to validate the demo quickly."
  type        = number
  default     = 5000
}

# Slow-resource demo knobs (v2d parallel-apply demo).
#
# Two independent `time_sleep` resources live at the root, slow_a and
# slow_b. Each one's `triggers` map references its own version variable, 
# so updating slow_a_version in terraform.tfvars forces ONLY slow_a to 
# be replaced (and slow_b's plan stays no-op, and vice versa). The
# disjoint write sets are exactly what the v2 reservation matrix is
# designed to let through concurrently:
#
#   - terminal A (via .tfvars): slow_a_version="v2" → write_set = [time_sleep.slow_a]
#   - terminal B (via .tfvars): slow_b_version="v2" → write_set = [time_sleep.slow_b]
#
# Both runs acquire write reservations on different addresses; neither
# blocks the other. With slow_duration=30s, each apply takes ~30s of
# wall-clock; running them in parallel finishes in ~30s total instead
# of ~60s serial — and a vanilla `terraform apply` issued in a third
# terminal during this window gets "Error acquiring the state lock"
# because terraform locks the whole state.

variable "slow_a_version" {
  description = "Bump (e.g. v1 -> v2) to force time_sleep.slow_a to be replaced. Used by the v2d parallel-apply demo."
  type        = string
  default     = "v1"
}

variable "slow_b_version" {
  description = "Bump (e.g. v1 -> v2) to force time_sleep.slow_b to be replaced. Used by the v2d parallel-apply demo."
  type        = string
  default     = "v1"
}

# slow_duration is deliberately NOT a variable.
#
# Earlier iterations of this file exposed it as `var.slow_duration`,
# which produced a footgun: bootstrapping with one duration and then
# running the demo with another forces an in-place UPDATE of the
# non-bumped slow resource (create_duration drift), pulling it into
# the write_set even though its trigger didn't change. That breaks
# the "disjoint write_set" invariant the parallel-apply demo depends
# on. Hardcoding it removes the drift class entirely.
#
# 30s is the smallest value that visibly demonstrates concurrent
# execution to a human watching two terminals; 1-5s blurs together
# and 60s+ is annoying. If you need a faster iteration loop while
# hacking on kl itself, edit this line locally — that's
# considered a code change, not a configuration knob.
locals {
  slow_duration = "30s"
}

# ---------------------------------------------------------------------------
# A small graph of "interesting" root-scope resources so the demo also
# exercises the dependency edges across resource types, not just the
# parallel herd.
# ---------------------------------------------------------------------------

resource "random_pet" "deployment_name" {
  length    = 3
  separator = "-"
}

resource "random_id" "deployment_id" {
  byte_length = 16

  keepers = {
    name = random_pet.deployment_name.id
  }
}

resource "null_resource" "deployment_marker" {
  triggers = {
    name = random_pet.deployment_name.id
    id   = random_id.deployment_id.hex
  }
}

# ---------------------------------------------------------------------------
# The herd: two child module instances, each producing 2 * var.size
# resources, demonstrating module_path normalization at scale and
# inter-module dependency edges.
# ---------------------------------------------------------------------------

module "primary_herd" {
  source = "./modules/herd"
  size   = var.size
  prefix = "${random_pet.deployment_name.id}-primary"
}

# A second herd that depends on the first via its leader, exercising
# cross-module dependency edges. Stays at size=1 because its only job
# is to demonstrate module_path rendering, not pile on more rows.
module "shadow_herd" {
  source = "./modules/herd"
  size   = 1
  prefix = "${module.primary_herd.leader}-shadow"
}

# Slow resources moved to slow_a.tf / slow_b.tf for file-scoped plan demos.

resource "null_resource" "summary" {
  triggers = {
    deployment     = random_pet.deployment_name.id
    primary_size   = module.primary_herd.size
    primary_leader = module.primary_herd.leader
    shadow_leader  = module.shadow_herd.leader
    note           = "kl-apply-demo2" # <-- add this
  }
}

# ---------------------------------------------------------------------------
# KL:
# Outputs (4 total).
# real    0m36.336s
# user    2m6.220s
# sys     0m13.494s
# TERRAFORM:
# real    0m40.137s
# user    2m2.970s
# sys     0m12.456s
# TERRAFORM v1.11.1
# ---------------------------------------------------------------------------

output "deployment_name" {
  value = random_pet.deployment_name.id
}

output "deployment_id" {
  value = random_id.deployment_id.hex
}

output "primary_size" {
  value = module.primary_herd.size
}

output "shadow_leader" {
  value = module.shadow_herd.leader
}
