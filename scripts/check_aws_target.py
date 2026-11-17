#!/usr/bin/env python3
"""Read-only preflight: refuse to deploy if the AWS account, region or
instance differ from the reviewed target in config/aws-targets.json, or if
the instance is not online in SSM. Prints KEY=VALUE lines for the workflow."""
import argparse
import json
import subprocess
import sys
from pathlib import Path


def aws(*args):
    return subprocess.check_output(['aws', *args, '--output', 'text'], text=True).strip()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--environment', choices=['nonprod'], required=True)
    args = parser.parse_args()
    target = json.loads((Path(__file__).resolve().parents[1] / 'config/aws-targets.json').read_text())[args.environment]

    account = aws('sts', 'get-caller-identity', '--query', 'Account')
    if account != target['account_id']:
        sys.exit(f"Authenticated AWS account {account} is not the {args.environment} account {target['account_id']}")

    state, public_ip = aws('ec2', 'describe-instances', '--region', target['region'],
                           '--instance-ids', target['instance_id'],
                           '--query', 'Reservations[0].Instances[0].[State.Name,PublicIpAddress]').split()
    if state != 'running':
        sys.exit(f"Instance {target['instance_id']} is {state}, not running")

    ping = aws('ssm', 'describe-instance-information', '--region', target['region'],
               '--filters', f"Key=InstanceIds,Values={target['instance_id']}",
               '--query', 'InstanceInformationList[0].PingStatus')
    if ping != 'Online':
        sys.exit(f"Instance {target['instance_id']} is not online in SSM (status: {ping})")

    print(f"public_ip={public_ip}")
    print(f"instance_id={target['instance_id']}")
    print(f"transfer_bucket={target['transfer_bucket']}")
    print(f"tango_dir={target['tango_dir']}")
    print('AWS identity, instance and SSM verified', file=sys.stderr)


if __name__ == '__main__':
    main()
