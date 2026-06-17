# Points at the default local compose stack from `docker-compose.yml`.
#
# Out of the box this example assumes:
#
#   cp .env.example .env
#   docker-compose up --build -d
#
# That default stack exposes the runtime on `http://localhost:8080` with open
# auth, so `terraform init` works here without extra bootstrap or token setup.
#
# If you want to run this example against the fuller control-plane stack instead,
# reconfigure the backend with your prod-like runtime URL and the credentials
# created during control-plane bootstrap.

terraform {
  backend "http" {
    address        = "http://localhost:8080/states/big-state"
    lock_address   = "http://localhost:8080/states/big-state"
    unlock_address = "http://localhost:8080/states/big-state"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
