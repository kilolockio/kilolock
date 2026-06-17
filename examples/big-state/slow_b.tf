resource "time_sleep" "slow_b" {
  create_duration  = local.slow_duration
  destroy_duration = "0s"

  triggers = {
    version = var.slow_b_version
  }
}

locals {
  test = "2s"
}
