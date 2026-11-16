#!/usr/bin/env bash
# Packs infra/ansible with this deploy's settings, uploads it to the ops
# bucket and runs it on the server through AWS SSM Run Command (no SSH).
# Prints the full Ansible log and fails if the playbook fails.
set -euo pipefail
: "${INSTANCE_ID:?}" "${HOSTNAME_PUBLIC:?}" "${OPS_BUCKET:?}" "${PARAM_PATH:?}" "${ECR_URL:?}" "${IMAGE:?}" "${AWS_REGION:?}"

echo "Waiting for ${INSTANCE_ID} to be online in AWS Systems Manager..."
status=""
for _ in $(seq 1 60); do
  status=$(aws ssm describe-instance-information --filters "Key=InstanceIds,Values=${INSTANCE_ID}" \
    --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || true)
  [ "$status" = "Online" ] && break
  sleep 10
done
if [ "$status" != "Online" ]; then
  echo "::error::The server did not register with SSM in 10 minutes (status: ${status:-none})."
  exit 1
fi

work=$(mktemp -d)
cp -r infra/ansible "$work/ansible"
jq -n \
  --arg image "$IMAGE" --arg host "$HOSTNAME_PUBLIC" --arg bucket "$OPS_BUCKET" --arg params "$PARAM_PATH" \
  --arg region "$AWS_REGION" --arg registry "${ECR_URL%%/*}" \
  '{tango_image: $image, hostname_public: $host, ops_bucket: $bucket, param_path: $params, aws_region: $region,
    ecr_registry: $registry, tango_environment: "nonprod"}' > "$work/ansible/vars.json"
tar -C "$work" -czf "$work/bundle.tgz" ansible
key="deploy/bundles/${GITHUB_SHA:-manual}-$(date +%s).tgz"
aws s3 cp --only-show-errors "$work/bundle.tgz" "s3://${OPS_BUCKET}/${key}"
url=$(aws s3 presign "s3://${OPS_BUCKET}/${key}" --expires-in 3600)

jq -n --arg url "$url" '{
  commands: [
    "set -e",
    "mkdir -p /opt/tango-deploy && cd /opt/tango-deploy && rm -rf ansible bundle.tgz",
    ("curl -fsSL \"" + $url + "\" -o bundle.tgz"),
    "tar xzf bundle.tgz",
    "bash ansible/run.sh"
  ],
  executionTimeout: ["3600"]
}' > "$work/params.json"

cmd=$(aws ssm send-command --instance-ids "$INSTANCE_ID" --document-name AWS-RunShellScript \
  --comment "tango deploy ${GITHUB_SHA:-manual}" --parameters "file://$work/params.json" \
  --output-s3-bucket-name "$OPS_BUCKET" --output-s3-key-prefix "deploy/ssm-logs" \
  --query Command.CommandId --output text)
echo "SSM command ${cmd} started; Ansible is running on the server..."

result="Pending"
for _ in $(seq 1 360); do
  result=$(aws ssm get-command-invocation --command-id "$cmd" --instance-id "$INSTANCE_ID" \
    --query Status --output text 2>/dev/null || echo Pending)
  case "$result" in
    Success|Failed|Cancelled|TimedOut) break ;;
  esac
  sleep 10
done

mkdir -p "$work/logs"
aws s3 cp --only-show-errors --recursive "s3://${OPS_BUCKET}/deploy/ssm-logs/${cmd}/" "$work/logs/" || true
for f in $(find "$work/logs" -type f -name stdout | sort) $(find "$work/logs" -type f -name stderr | sort); do
  echo "::group::$(basename "$f") (server)"
  cat "$f"
  echo "::endgroup::"
done
if [ "$result" != "Success" ]; then
  aws ssm get-command-invocation --command-id "$cmd" --instance-id "$INSTANCE_ID" \
    --query '{status:Status,stdout:StandardOutputContent,stderr:StandardErrorContent}' --output text || true
  echo "::error::Ansible on the server finished with status ${result}"
  exit 1
fi
echo "Ansible finished successfully."
