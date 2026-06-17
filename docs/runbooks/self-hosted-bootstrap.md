# Self-hosted bootstrap

This runbook covers the **self-hosted OSS** bootstrap flow for local or
operator-managed deployments using `docker-compose.prodlike.yml`.

It is intentionally different from the Cloud-only GCP bootstrap flow, which
uses a one-time Cloud Run job and bootstrap artifact handling.

## Goal

Initialize the control-plane metadata database once, capture the bootstrap
operator token, and create the first workspace, environment, and runtime token
for Terraform or `kl`.

## When to use this

Use this runbook when you are:

- self-hosting Kilolock OSS
- running `docker-compose.prodlike.yml`
- using `KL_INIT_MODE=prod`

Do **not** use this runbook for the managed/private Cloud deployment model.

## 1) Start the prod-like stack

From repo root:

```bash
cp .env.example .env
docker-compose -f docker-compose.prodlike.yml up --build -d
```

In prod init mode, `kld` will refuse to serve requests until initialization is
completed in the control-plane metadata DB.

By default, prod mode expects strict transport
(`KL_PROD_TLS_REQUIRED=true`). If you are doing local non-regulated testing
without TLS, explicitly set:

```bash
KL_PROD_TLS_REQUIRED=false
```

## 2) Run one-time migrate + init

Execute the control-plane migration first, then initialize the system:

```bash
docker-compose -f docker-compose.prodlike.yml exec klc klc migrate
docker-compose -f docker-compose.prodlike.yml exec klc klc init \
  --tenant self-hosted \
  --tenant-name "Self Hosted" \
  --token-name operator-bootstrap
```

`init` prints the bootstrap token once. Treat it like a secret and store it
securely.

The prod-like compose expects `KL_CONTROL_TOKEN` for local runs because the
control plane refuses to start in prod mode without an operator API token.

## 3) Verify services respond

After `init` succeeds:

- runtime API: `http://localhost:8080`
- control-plane API: `http://localhost:8090`
- control UI: `http://localhost:8090/portal`

## 4) Create the first workspace, environment, and token

Open the control UI and paste the bootstrap token from `init`.

Then create:

- a workspace
- an environment inside that workspace
- a token for that environment

More explicitly:

1. Create the workspace and copy its `workspace_id` (`ws_...`).
2. Create an environment under that workspace.
3. Load that workspace in `Environments by Workspace` and copy the environment
   `env_public_id` (`env_...`).
4. Create a token using that same `workspace_id` and `env_public_id`.
5. Copy the raw token secret (`kl_...`) when shown.

That is the normal self-hosted onboarding path before using Terraform or `kl`
against the runtime API.

Control-plane API reference snippets for onboarding and operator flows live in:

- `docs/runbooks/control-api.md`

## 5) Point Terraform at the prod-like runtime

For sample projects that default to the quick local backend, copy the
prod-like example backend file into place before running `terraform init`:

```bash
cp examples/local-backend/backend.tf.prodlike examples/big-state/backend.tf
rm -rf examples/big-state/.terraform examples/big-state/.terraform.lock.hcl
(cd examples/big-state && terraform init)
```

Use backend addresses shaped like:

```text
http://localhost:8080/states/{workspace_id}/{env_public_id}/{state_name}
```

Where:

- `workspace_id` is the workspace slug (`ws_...`)
- `env_public_id` comes from the environment row in the control UI / control API
- `state_name` is your Terraform state name, for example `big-state`

## Recovery notes

### Stack was initialized against an older migration baseline

If you previously ran this stack before the current migration baseline was
squashed, wipe volumes and restart:

```bash
docker-compose -f docker-compose.prodlike.yml down -v
docker-compose -f docker-compose.prodlike.yml up --build -d
```

Then rerun migrate + init.

### Runtime still refuses to serve after init

- verify `klc init` completed successfully
- verify the metadata DB used by `klc` and `kld` is the same one
- inspect compose logs for `kl` and `klc`

### Lost bootstrap token

Treat this as an operator credential recovery problem rather than rerunning
`init` blindly. Inspect the control-plane database state first and recover
through established operator-secret procedures.
