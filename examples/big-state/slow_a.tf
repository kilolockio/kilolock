resource "time_sleep" "slow_a" {
  create_duration  = local.slow_duration
  destroy_duration = "0s"

  triggers = {
    version = var.slow_a_version
  }
}
