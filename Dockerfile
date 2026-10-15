# SPDX-FileCopyrightText: 2026 Reza Jelveh
# SPDX-License-Identifier: Apache-2.0
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/hami-mock-monitor ./cmd/hami-mock-monitor

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/hami-mock-monitor /hami-mock-monitor
EXPOSE 9394
ENTRYPOINT ["/hami-mock-monitor"]
