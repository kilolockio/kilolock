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
    # address        = "https://api.kilolock.cloud/v1/states/ws_52a8ab8f3944/env_0316a5d4a432/small-state"
    # lock_address   = "https://api.kilolock.cloud/v1/states/ws_52a8ab8f3944/env_0316a5d4a432/small-state"
    # unlock_address = "https://api.kilolock.cloud/v1/state-unlock/ws_52a8ab8f3944/env_0316a5d4a432/small-state"
    # lock_method    = "LOCK"
    # unlock_method  = "POST"

    # username = "ws_52a8ab8f3944"
    # password = "klp_LIGeDfhyOV3W501yOxg3oSVOfjCylS9qj-n2fX67bBE"
    # # password = "kl_t3YH4tDv_7r0UYueJWtq7YqSiKvIZrt_7kqcZhQGUmc"
    # password = "kl_K4vg_vRznl0BGhKPg63sWatwDT5bZz1Z8-C0NetFLDU"
    # password = "klp_aJpmno785tUAdTpslX2g2_dbzauwk7M9lcYmhyDlPkE"

    address        = "http://localhost:8080/v1/states/big-state"
    lock_address   = "http://localhost:8080/v1/states/big-state"
    unlock_address = "http://localhost:8080/v1/states/big-state"
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
