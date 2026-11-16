#!/usr/bin/env python3
"""Nightly database backup: pg_dump from the postgres container, gzip, upload
to s3://<bucket>/backups/. The bucket expires old backups automatically."""
import datetime
import gzip
import os
import shutil
import subprocess
import sys

import boto3

bucket, region, tango_dir = sys.argv[1], sys.argv[2], sys.argv[3]
stamp = datetime.datetime.utcnow().strftime("%Y-%m-%dT%H%M%SZ")
tmp = f"/var/tmp/tango-{stamp}.sql.gz"
dump = subprocess.Popen(
    ["docker", "compose", "--project-directory", tango_dir, "exec", "-T", "postgres",
     "pg_dump", "-U", "tango", "-d", "tango", "--no-owner"],
    stdout=subprocess.PIPE)
with gzip.open(tmp, "wb") as out:
    shutil.copyfileobj(dump.stdout, out)
if dump.wait() != 0:
    os.remove(tmp)
    sys.exit("pg_dump failed")
boto3.client("s3", region_name=region).upload_file(tmp, bucket, f"backups/{stamp}.sql.gz")
os.remove(tmp)
print(f"uploaded s3://{bucket}/backups/{stamp}.sql.gz")
