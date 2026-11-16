#!/usr/bin/env python3
"""Print every SSM parameter under a path as JSON {name: value} (decrypted).

Used by Ansible on the server; the instance role allows reading only this
environment's path."""
import json
import sys

import boto3

path, region = sys.argv[1].rstrip("/"), sys.argv[2]
ssm = boto3.client("ssm", region_name=region)
out = {}
for page in ssm.get_paginator("get_parameters_by_path").paginate(Path=path, WithDecryption=True):
    for p in page["Parameters"]:
        out[p["Name"].rsplit("/", 1)[-1]] = p["Value"]
json.dump(out, sys.stdout)
