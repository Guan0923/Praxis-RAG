#!/bin/bash
# Least-privilege MySQL account for the Debezium binlog reader.
#
# The application account (MYSQL_USER) only needs DML on its own schema; the
# replication client needs global privileges, so it gets its own user instead
# of widening the application account.
#
# Runs once, on first initialisation of an empty data directory.
set -euo pipefail

if [ -z "${RAG_MYSQL_DEBEZIUM_PASSWORD:-}" ]; then
    echo "debezium-init: RAG_MYSQL_DEBEZIUM_PASSWORD is unset; skipping CDC user creation."
    exit 0
fi

: "${RAG_MYSQL_DEBEZIUM_USER:=debezium}"

mysql --protocol=socket -uroot -p"${MYSQL_ROOT_PASSWORD}" <<EOSQL
CREATE USER IF NOT EXISTS '${RAG_MYSQL_DEBEZIUM_USER}'@'%' IDENTIFIED BY '${RAG_MYSQL_DEBEZIUM_PASSWORD}';
-- Debezium MySQL connector requirements: read the tables (SELECT), flush/snapshot
-- coordination (RELOAD, SHOW DATABASES) and read the binary log (REPLICATION ...).
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT
    ON *.* TO '${RAG_MYSQL_DEBEZIUM_USER}'@'%';
FLUSH PRIVILEGES;
EOSQL

echo "debezium-init: CDC user '${RAG_MYSQL_DEBEZIUM_USER}' ready."
