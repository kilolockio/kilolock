# OSS first release checklist

Use this before the first public push or first tagged release of `kilolock`.

## Repository shape

- [ ] README reflects the current `kl` / `kld` / `klc` split
- [ ] GitHub Pages site under `docs/` renders correctly
- [ ] no hosted-product-only docs, portal references, or private deployment instructions remain by mistake
- [ ] license, trademark, contributing, maintainers, and code of conduct files are present
- [ ] community templates exist (`issue`, `PR`, `security`)

## Build and test

- [ ] `go test ./...`
- [ ] `make build`
- [ ] `docker-compose up --build -d`
- [ ] quick smoke against the default runtime-only HTTP backend succeeds
- [ ] `docker-compose -f docker-compose.prodlike.yml up --build -d`
- [ ] `docker-compose -f docker-compose.prodlike.yml exec klc klc migrate`
- [ ] `docker-compose -f docker-compose.prodlike.yml exec klc klc init --tenant self-hosted --tenant-name "Self Hosted" --token-name operator-bootstrap`
- [ ] `docs/runbooks/self-hosted-bootstrap.md` matches the shipped self-hosted bootstrap flow

## Product sanity

- [ ] `kl` CLI works without direct DB access
- [ ] `kld` owns runtime DB connectivity
- [ ] `klc` owns control-plane DB connectivity
- [ ] scoped/orchestrated apply behavior matches current docs
- [ ] resource query/history/repair flow is demonstrated at least once

## Publishing

- [ ] GitHub Actions are green on `main`
- [ ] GitHub Pages is enabled for `docs/`
- [ ] first tag/version naming is decided
- [ ] release notes summarize current strengths and known gaps
- [ ] known limitations are written honestly (especially around evolving apply semantics)
