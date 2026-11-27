# The integration fixture image: the fake model and GitHub endpoints, plus the
# released CLI so the same pod can exercise both clients against the installed
# service. This image is never published.
FROM golang:1.27.4-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/kubectl-page/ cmd/kubectl-page/
COPY test/e2e/investigation/fake/ test/e2e/investigation/fake/
RUN CGO_ENABLED=0 go build -trimpath -o /out/fake ./test/e2e/investigation/fake
RUN CGO_ENABLED=0 go build -trimpath -o /out/kubectl-page ./cmd/kubectl-page

FROM alpine:3.23.8
RUN apk add --no-cache ca-certificates-bundle=20261020-r0
COPY --from=build /out/fake /usr/local/bin/fake
COPY --from=build /out/kubectl-page /usr/local/bin/kubectl-page
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/fake"]
