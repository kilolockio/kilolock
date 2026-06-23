# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build
WORKDIR /src

ARG KL_GO_TAGS=""
ARG VERSION="0.0.0-dev"
ARG GIT_COMMIT="unknown"
ARG GIT_DIRTY="clean"
ARG BUILD_TIME=""

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG KL_LDFLAGS="-s -w -X github.com/kilolockio/kilolock/pkg/buildinfo.Version=${VERSION} -X github.com/kilolockio/kilolock/pkg/buildinfo.Commit=${GIT_COMMIT} -X github.com/kilolockio/kilolock/pkg/buildinfo.BuildTime=${BUILD_TIME} -X github.com/kilolockio/kilolock/pkg/buildinfo.Dirty=${GIT_DIRTY}"
RUN CGO_ENABLED=0 GOOS=linux go build ${KL_GO_TAGS:+-tags=${KL_GO_TAGS}} -trimpath -ldflags="${KL_LDFLAGS}" -o /out/kl ./cmd/kl
RUN CGO_ENABLED=0 GOOS=linux go build ${KL_GO_TAGS:+-tags=${KL_GO_TAGS}} -trimpath -ldflags="${KL_LDFLAGS}" -o /out/kld ./cmd/kld
RUN CGO_ENABLED=0 GOOS=linux go build ${KL_GO_TAGS:+-tags=${KL_GO_TAGS}} -trimpath -ldflags="${KL_LDFLAGS}" -o /out/klc ./cmd/klc

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /

COPY --from=build /out/kl /usr/local/bin/kl
COPY --from=build /out/kld /usr/local/bin/kld
COPY --from=build /out/klc /usr/local/bin/klc

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/kld"]
CMD ["serve"]
