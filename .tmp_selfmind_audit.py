import json
import sqlite3
import sys
from datetime import datetime, timezone


def convert_timestamp(value):
    if value is None:
        return None
    seconds = float(value)
    if seconds > 1e17:
        seconds /= 1e9
    elif seconds > 1e14:
        seconds /= 1e6
    elif seconds > 1e11:
        seconds /= 1e3
    return datetime.fromtimestamp(seconds, timezone.utc).isoformat()


def main() -> None:
    db_path = sys.argv[1]
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    tables = [
        row["name"]
        for row in conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
        )
    ]
    if len(sys.argv) > 2 and sys.argv[2] == "samples":
        result = {}
        for table, column in (
            ("task_runs", "started_at"),
            ("task_events", "created_at"),
            ("approval_requests", "created_at"),
            ("maintenance_jobs", "created_at"),
            ("outbound_messages", "created_at"),
        ):
            rows = conn.execute(
                f'SELECT {column} FROM "{table}" ORDER BY {column} DESC LIMIT 3'
            ).fetchall()
            result[table] = [
                {"raw": row[column], "utc": convert_timestamp(row[column])}
                for row in rows
            ]
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return

    result = {}
    for table in tables:
        columns = [dict(row) for row in conn.execute(f'PRAGMA table_info("{table}")')]
        count = conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
        result[table] = {"count": count, "columns": columns}
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
