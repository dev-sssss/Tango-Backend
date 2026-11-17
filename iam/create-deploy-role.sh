#!/usr/bin/env bash
# One-time: create the GitHub OIDC deploy role for dev-sssss/Tango-Backend in
# the nonprod account. Run it once in AWS CloudShell (or any shell with admin
# credentials for account 521503820723). Safe to run again: it updates the
# trust and policy in place.
#
# Same shape as iam/github-deploy-role.yml (Purpose=infrastructure) in
# dev-sssss/ludo_nakama-ansible:
#   - only GitHub Actions jobs of THIS repository, running in its "nonprod"
#     environment, can assume it (exact immutable-ID subject, no wildcards)
#   - it can open SSM sessions to the one nonprod instance and use the SSM
#     transfer bucket; nothing else
set -euo pipefail

ACCOUNT=521503820723
REGION=ap-south-1
INSTANCE=i-060485c20a979c038
BUCKET=ludo-nonprod-ssm-${ACCOUNT}-${REGION}
ROLE=tango-nonprod-infrastructure
OWNER_ID=296378927        # dev-sssss
REPO_ID=1459562044        # dev-sssss/Tango-Backend
SUBJECT="repo:dev-sssss@${OWNER_ID}/Tango-Backend@${REPO_ID}:environment:nonprod"
PROVIDER="arn:aws:iam::${ACCOUNT}:oidc-provider/token.actions.githubusercontent.com"

[ "$(aws sts get-caller-identity --query Account --output text)" = "$ACCOUNT" ] ||
  { echo "Wrong AWS account: switch to the nonprod account ($ACCOUNT)" >&2; exit 1; }
aws iam get-open-id-connect-provider --open-id-connect-provider-arn "$PROVIDER" >/dev/null ||
  { echo "GitHub OIDC provider missing in this account" >&2; exit 1; }

tmp=$(mktemp -d)
cat > "$tmp/trust.json" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "$PROVIDER"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {"StringEquals": {
      "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
      "token.actions.githubusercontent.com:sub": "$SUBJECT"
    }}
  }]
}
EOF
cat > "$tmp/policy.json" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
     "Resource": "arn:aws:s3:::$BUCKET/*"},
    {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
     "Resource": "arn:aws:s3:::$BUCKET"},
    {"Effect": "Allow", "Action": "ssm:StartSession",
     "Resource": ["arn:aws:ec2:$REGION:$ACCOUNT:instance/$INSTANCE",
                  "arn:aws:ssm:$REGION:$ACCOUNT:document/SSM-SessionManagerRunShell"]},
    {"Effect": "Allow", "Action": ["ssm:GetCommandInvocation", "ssm:DescribeInstanceInformation", "ec2:DescribeInstances"],
     "Resource": "*"},
    {"Effect": "Allow", "Action": ["ssm:TerminateSession", "ssm:ResumeSession"],
     "Resource": "arn:aws:ssm:$REGION:$ACCOUNT:session/*"},
    {"Effect": "Allow", "Action": "ssmmessages:OpenDataChannel", "Resource": "*"}
  ]
}
EOF

if aws iam get-role --role-name "$ROLE" >/dev/null 2>&1; then
  aws iam update-assume-role-policy --role-name "$ROLE" --policy-document "file://$tmp/trust.json"
else
  aws iam create-role --role-name "$ROLE" --max-session-duration 3600 \
    --assume-role-policy-document "file://$tmp/trust.json" \
    --description "GitHub Actions SSM deploy for dev-sssss/Tango-Backend (nonprod)" \
    --tags Key=ManagedBy,Value=tango-pipeline Key=Repository,Value=dev-sssss/Tango-Backend >/dev/null
fi
aws iam put-role-policy --role-name "$ROLE" --policy-name EnvironmentDeploy --policy-document "file://$tmp/policy.json"
rm -rf "$tmp"
echo "Deploy role ready: arn:aws:iam::${ACCOUNT}:role/${ROLE}"
