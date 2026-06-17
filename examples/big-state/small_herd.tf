module "small_herd" {
  source = "./modules/herd"
  size   = 24
  prefix = "${random_pet.deployment_name.id}-small"
}

# module "small_herd2" {
#   source = "./modules/herd"
#   size   = 12
#   prefix = "${random_pet.deployment_name.id}-big"
# }
