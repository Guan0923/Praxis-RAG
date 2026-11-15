#!/bin/bash
# Least-privilege MySQL accounts used by the TWO Debezium connectors hosted by
# the distributed Kafka Connect cluster:
#   * ${RAG_MYSQL_DEBEZIUM_USER}       -> document connector (document_jobs)
#   * ${RAG_MYSQL_DEBEZIUM_CACHE_USER} -> cache connector (8 read-model tables)
#
# The accounts are intentionally separate: besides least privilege, a
# credential/privilege incident on one capture path must not stop the other. It
# also makes the failed-task recovery drill possible (lock the cache account,
# observe only the cache task fail, unlock it and let the supervisor restart
# that task).
#
# The application account (MYSQL_USER) only needs DML on its own schema; the
# replication clients need global privileges, so they get their own users.
#
# Idempotent: CREATE USER IF NOT EXISTS + GRANT can be re-run safely. This file
# runs once on first initialisation of an empty data directory; for an existing
# volume run scripts/start-stack.ps1 (it converges the same accounts) or the
# same statements manually (see deploy/kafka-connect/README.md).
set -euo pipefail

document_user="${RAG_MYSQL_DEBEZIUM_USER:-debezium}"
document_password="${RAG_MYSQL_DEBEZIUM_PASSWORD:-}"
cache_user="${RAG_MYSQL_DEBEZIUM_CACHE_USER:-debezium_cache}"
cache_password="${RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD:-}"
# The passwords are interpolated into single-quoted SQL literals. Double any
# embedded quote so a generated secret cannot break (or inject into) the SQL.
document_password="${document_password//\'/\'\'}"
cache_password="${cache_password//\'/\'\'}"

if [ -z "${document_password}" ]; then
    echo "debezium-init: RAG_MYSQL_DEBEZIUM_PASSWORD is unset; skipping document CDC user creation." >&2
else
    mysql --protocol=socket -uroot -p"${MYSQL_ROOT_PASSWORD}" <<EOSQL
CREATE USER IF NOT EXISTS '${document_user}'@'%' IDENTIFIED BY '${document_password}';
-- Debezium MySQL connector requirements:
--   SELECT             read the captured tables during the initial snapshot
--   RELOAD             flush/coordinate the snapshot
--   SHOW DATABASES     read the schema history / database include list
--   REPLICATION SLAVE  open the binlog reader
--   REPLICATION CLIENT read SHOW MASTER STATUS (binlog position and GTID)
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT
    ON *.* TO '${document_user}'@'%';
FLUSH PRIVILEGES;
EOSQL
fi

if [ -z "${cache_password}" ]; then
    echo "debezium-init: RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD is unset; skipping cache CDC user creation." >&2
elif [ "${cache_user}" = "${document_user}" ]; then
    echo "debezium-init: cache user equals the document user; set RAG_MYSQL_DEBEZIUM_CACHE_USER to a distinct account." >&2
else
    mysql --protocol=socket -uroot -p"${MYSQL_ROOT_PASSWORD}" <<EOSQL
CREATE USER IF NOT EXISTS '${cache_user}'@'%' IDENTIFIED BY '${cache_password}';
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT
    ON *.* TO '${cache_user}'@'%';
FLUSH PRIVILEGES;
EOSQL
fi

# Both connectors need row-based binlog events with full before/after images:
# the cache connector publishes delete events from the before image, and the
# document connector relies on the captured-column diff for
# skip.messages.without.change. docker-compose.yml sets --binlog-row-image=FULL
# explicitly; this check only surfaces a misconfigured existing server.
ROW_IMAGE="$(mysql --protocol=socket -uroot -p"${MYSQL_ROOT_PASSWORD}" -N -B -e 'SELECT @@GLOBAL.binlog_row_image;')"
if [ "${ROW_IMAGE}" != "FULL" ]; then
    echo "debezium-init: WARNING binlog_row_image=${ROW_IMAGE}; Debezium needs FULL for delete/update images. Set --binlog-row-image=FULL and restart MySQL." >&2
fi

echo "debezium-init: CDC users '${document_user}' (document) and '${cache_user}' (cache) ready (binlog_row_image=${ROW_IMAGE})."
