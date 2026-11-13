#!/bin/bash
# Least-privilege MySQL account shared by the TWO Debezium connectors
# (debezium = task connector, debezium-cache = cache connector).
#
# The application account (MYSQL_USER) only needs DML on its own schema; the
# replication client needs global privileges, so it gets its own user instead
# of widening the application account. Both connector instances use the same
# account: they differ by database.server.id and offset storage, not by grants.
#
# Idempotent: CREATE USER IF NOT EXISTS + GRANT can be re-run safely. This file
# runs once on first initialisation of an empty data directory; for an existing
# volume run the same GRANTs manually (see deploy/debezium/README.md).
set -euo pipefail

if [ -z "${RAG_MYSQL_DEBEZIUM_PASSWORD:-}" ]; then
    echo "debezium-init: RAG_MYSQL_DEBEZIUM_PASSWORD is unset; skipping CDC user creation."
    exit 0
fi

: "${RAG_MYSQL_DEBEZIUM_USER:=debezium}"

mysql --protocol=socket -uroot -p"${MYSQL_ROOT_PASSWORD}" <<EOSQL
CREATE USER IF NOT EXISTS '${RAG_MYSQL_DEBEZIUM_USER}'@'%' IDENTIFIED BY '${RAG_MYSQL_DEBEZIUM_PASSWORD}';
-- Debezium MySQL connector requirements, identical for the task and cache
-- connectors:
--   SELECT             read the captured tables during the initial snapshot
--   RELOAD             flush/coordinate the snapshot
--   SHOW DATABASES     read the schema history / database include list
--   REPLICATION SLAVE  open the binlog reader
--   REPLICATION CLIENT read SHOW MASTER STATUS (binlog position and GTID)
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT
    ON *.* TO '${RAG_MYSQL_DEBEZIUM_USER}'@'%';
FLUSH PRIVILEGES;
EOSQL

# Both connectors need row-based binlog events with full before/after images:
# the cache connector publishes delete events from the before image, and the
# task connector relies on the captured-column diff for
# skip.messages.without.change. docker-compose.yml sets --binlog-row-image=FULL
# explicitly; this check only surfaces a misconfigured existing server.
ROW_IMAGE="$(mysql --protocol=socket -uroot -p"${MYSQL_ROOT_PASSWORD}" -N -B -e 'SELECT @@GLOBAL.binlog_row_image;')"
if [ "${ROW_IMAGE}" != "FULL" ]; then
    echo "debezium-init: WARNING binlog_row_image=${ROW_IMAGE}; Debezium needs FULL for delete/update images. Set --binlog-row-image=FULL and restart MySQL." >&2
fi

echo "debezium-init: CDC user '${RAG_MYSQL_DEBEZIUM_USER}' ready for both Debezium connectors (binlog_row_image=${ROW_IMAGE})."
