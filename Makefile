.PHONY: test test-integration lint

test:  ## unit tests: the manifest, the OpenAPI document and the routes, merge safety, the configuration and the publisher check
	go vet ./... && go test ./...

test-integration:  ## L2 against PostgreSQL and the bus (BE_SDK_TEST_PG_ADMIN, BE_SDK_TEST_NATS_URL)
	go test -count=1 -tags integration ./itest/

lint:
	brickkit lint
