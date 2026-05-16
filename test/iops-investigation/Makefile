SHELL := /bin/bash

NATS_IMAGE  := $(or $(IOPS_RIG_NATS_IMAGE),nats:2.12.6)
REPLICAS    := $(or $(IOPS_RIG_NATS_REPLICAS),3)
COMPOSE     := docker compose -f docker/docker-compose.yaml

.PHONY: up down reset image-digest

## up — start the NATS cluster (respects IOPS_RIG_NATS_REPLICAS; default 3)
up:
	$(COMPOSE) --profile r$(REPLICAS) up -d

## down — stop and remove all cluster containers (both profiles)
down:
	$(COMPOSE) --profile r3 --profile r5 down

## reset — drop volumes and restart with a fresh cluster
reset:
	$(COMPOSE) --profile r3 --profile r5 down -v
	$(COMPOSE) --profile r$(REPLICAS) up -d

## image-digest — print the resolved digest of the NATS image
image-digest:
	docker image inspect $(NATS_IMAGE) --format '{{index .RepoDigests 0}}'
