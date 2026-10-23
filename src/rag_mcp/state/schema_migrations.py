"""Small additive migrations for existing state databases."""

from sqlalchemy import inspect, text
from sqlalchemy.exc import DBAPIError


def migrate_soft_delete(engine):
    """Add the document tombstone without rewriting/deleting existing data.

    create_all does not alter existing tables. Concurrent service startup may
    race this ALTER; only an independently confirmed existing column is accepted.
    """
    if not inspect(engine).has_table("documents"):
        return
    if "del_flag" in {column["name"] for column in inspect(engine).get_columns("documents")}:
        return
    try:
        with engine.begin() as conn:
            conn.execute(text("ALTER TABLE documents ADD COLUMN del_flag BOOLEAN NOT NULL DEFAULT 0"))
    except DBAPIError:
        if "del_flag" not in {column["name"] for column in inspect(engine).get_columns("documents")}:
            raise
