# Local development loads .env; production reads real environment variables.
-include .env
export

PG_BIN      ?= /opt/homebrew/opt/postgresql@18/bin
PGDATA      ?= /opt/homebrew/var/postgresql@18
PGLOG       ?= /opt/homebrew/var/log/postgresql@18.log
DEV_DB      ?= starhane_fm
TEST_DB     ?= starhane_fm_test

.PHONY: dev build run-api run-crawler test fmt vet check \
        db-start db-stop db-status db-create db-drop \
        migrate-up migrate-down migrate-down-all migrate-status migrate-new \
        docker-build

# --- running -----------------------------------------------------------------

dev: run-api

run-api:
	@go run ./cmd/api

run-crawler:
	@go run ./cmd/crawler

build:
	@go build -o bin/api ./cmd/api
	@go build -o bin/crawler ./cmd/crawler
	@go build -o bin/migrate ./cmd/migrate

# --- checks ------------------------------------------------------------------

test:
	@go test ./...

fmt:
	@gofmt -l -w .

vet:
	@go vet ./...

check: fmt vet test

# --- local database ----------------------------------------------------------

db-start:
	@$(PG_BIN)/pg_ctl -D $(PGDATA) -l $(PGLOG) -o "-p 5432" start || true
	@$(PG_BIN)/pg_isready -p 5432

db-stop:
	@$(PG_BIN)/pg_ctl -D $(PGDATA) stop

db-status:
	@$(PG_BIN)/pg_isready -p 5432

db-create:
	@for db in $(DEV_DB) $(TEST_DB); do \
		if $(PG_BIN)/psql -d postgres -tAc "select 1 from pg_database where datname='$$db'" | grep -q 1; then \
			echo "$$db already exists"; \
		else \
			$(PG_BIN)/createdb "$$db" && echo "created $$db"; \
		fi; \
	done

db-drop:
	@$(PG_BIN)/dropdb --if-exists $(DEV_DB)
	@$(PG_BIN)/dropdb --if-exists $(TEST_DB)

# --- migrations --------------------------------------------------------------

migrate-up:
	@go run ./cmd/migrate up

# Rolls back one migration. Use migrate-down-all to unwind everything.
migrate-down:
	@go run ./cmd/migrate down

migrate-down-all:
	@go run ./cmd/migrate down --all

migrate-status:
	@go run ./cmd/migrate status

# make migrate-new name=add_something
migrate-new:
	@test -n "$(name)" || (echo "usage: make migrate-new name=add_something" && exit 1)
	@next=$$(printf "%06d" $$(( $$(ls db/migrations/*.up.sql 2>/dev/null | wc -l | tr -d ' ') + 1 ))); \
	touch "db/migrations/$${next}_$(name).up.sql" "db/migrations/$${next}_$(name).down.sql"; \
	echo "created db/migrations/$${next}_$(name).{up,down}.sql"

# --- docker ------------------------------------------------------------------

docker-build:
	@docker build -t starhane-fm-server .
