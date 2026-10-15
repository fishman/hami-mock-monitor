# SPDX-FileCopyrightText: 2026 Reza Jelveh
# SPDX-License-Identifier: Apache-2.0
GO ?= go
HELM ?= helm
IMG ?= hami-mock-monitor:dev
CHART := charts/hami-mock-monitor

.PHONY: all build test lint image chart-test

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/hami-mock-monitor ./cmd/hami-mock-monitor

test:
	$(GO) test ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || (echo "gofmt needed:"; gofmt -l cmd internal; exit 1)
	$(GO) vet ./...

image:
	docker build -t $(IMG) .

# Renders the chart with bind completion off and on.
chart-test:
	$(HELM) lint $(CHART)
	$(HELM) template test $(CHART) >/dev/null
	$(HELM) template test $(CHART) --set completeBinds=true --set serviceMonitor.enabled=true >/dev/null
