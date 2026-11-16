<#
  One-time setup for the Tango Cards deploy pipeline (Windows).

  Double-click scripts\setup-github.bat. It:
    1. installs GitHub CLI and AWS CLI if they are missing (winget)
    2. signs in to GitHub in the browser if needed
    3. creates an AWS role that only GitHub Actions of dev-sssss/Tango-Backend
       can use (no access keys are stored anywhere), using your AWS CLI
       "default" profile (your non-prod account)
    4. saves the role and region as repository variables
    5. opens the "Deploy nonprod" workflow page: press "Run workflow" there

  It creates nothing that costs money. The server is only created when you
  press "Run workflow".
#>
param(
  [string]$Owner = "dev-sssss",
  [string]$Repo = "Tango-Backend",
  [string]$AwsProfile = "default",
  [string]$Region = "ap-south-1"
)

# Native tools report failure through exit codes, which are checked after
# each call ("Stop" would turn harmless stderr output into errors on
# Windows PowerShell 5.1).
$ErrorActionPreference = "Continue"

function Step($msg) { Write-Host ""; Write-Host "==> $msg" -ForegroundColor Cyan }
function Fail($msg) { Write-Host ""; Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }

function Refresh-Path {
  $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
}

function Ensure-Tool($exe, $wingetId) {
  if (Get-Command $exe -ErrorAction SilentlyContinue) { return }
  Write-Host "Installing $wingetId ..."
  winget install --id $wingetId -e --silent --accept-package-agreements --accept-source-agreements | Out-Host
  Refresh-Path
  if (-not (Get-Command $exe -ErrorAction SilentlyContinue)) { Fail "$exe is still missing after installing $wingetId. Close this window and run the script again." }
}

function Write-Json($path, $text) {
  # AWS CLI needs UTF-8 without BOM.
  [IO.File]::WriteAllText($path, $text, (New-Object System.Text.UTF8Encoding($false)))
}

# ------------------------------------------------------------ tools
Step "Checking tools"
Ensure-Tool "gh" "GitHub.cli"
Ensure-Tool "aws" "Amazon.AWSCLI"

# ------------------------------------------------------------ GitHub
Step "GitHub sign-in"
gh auth status 2>$null | Out-Null
if ($LASTEXITCODE -ne 0) {
  gh auth login --web --git-protocol https --scopes "repo,workflow"
  if ($LASTEXITCODE -ne 0) { Fail "GitHub sign-in did not complete." }
}
# Exact owner/name casing matters for the AWS trust policy.
$FullRepo = (gh repo view "$Owner/$Repo" --json nameWithOwner --jq .nameWithOwner)
if ($LASTEXITCODE -ne 0 -or -not $FullRepo) { Fail "Cannot open GitHub repository $Owner/$Repo with this GitHub account." }
$FullRepo = $FullRepo.Trim()
Write-Host "Repository: $FullRepo"

# ------------------------------------------------------------ AWS
Step "AWS account (profile '$AwsProfile')"
$Account = (aws sts get-caller-identity --profile $AwsProfile --query Account --output text 2>$null)
if ($LASTEXITCODE -ne 0 -or -not $Account) {
  Write-Host "AWS CLI is not signed in for profile '$AwsProfile'. Enter your non-prod access key when asked."
  aws configure --profile $AwsProfile
  $Account = (aws sts get-caller-identity --profile $AwsProfile --query Account --output text)
  if ($LASTEXITCODE -ne 0) { Fail "AWS sign-in failed." }
}
$Account = $Account.Trim()
Write-Host "AWS account: $Account"

$Tmp = Join-Path $env:TEMP "tango-setup"
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null
$OidcUrl = "token.actions.githubusercontent.com"
$OidcArn = "arn:aws:iam::${Account}:oidc-provider/$OidcUrl"

Step "GitHub sign-in for AWS (OIDC provider)"
$providers = aws iam list-open-id-connect-providers --profile $AwsProfile --query "OpenIDConnectProviderList[].Arn" --output text
if ($providers -notmatch [regex]::Escape($OidcArn)) {
  aws iam create-open-id-connect-provider --profile $AwsProfile --url "https://$OidcUrl" --client-id-list sts.amazonaws.com --thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1 | Out-Null
  if ($LASTEXITCODE -ne 0) { Fail "Could not create the OIDC provider." }
}

$RoleName = "tango-nonprod-github-deploy"
$trust = @"
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Federated": "$OidcArn" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": { "${OidcUrl}:aud": "sts.amazonaws.com" },
      "StringLike": { "${OidcUrl}:sub": "repo:${FullRepo}:*" }
    }
  }]
}
"@
$iam = @"
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "TangoRolesOnly",
      "Effect": "Allow",
      "Action": ["iam:*Role*", "iam:*InstanceProfile*", "iam:*RolePolicy*", "iam:TagRole", "iam:UntagRole"],
      "Resource": [
        "arn:aws:iam::${Account}:role/tango-*",
        "arn:aws:iam::${Account}:instance-profile/tango-*"
      ]
    },
    {
      "Sid": "ReadIam",
      "Effect": "Allow",
      "Action": ["iam:Get*", "iam:List*"],
      "Resource": "*"
    },
    {
      "Sid": "ServiceLinkedRoles",
      "Effect": "Allow",
      "Action": "iam:CreateServiceLinkedRole",
      "Resource": "*"
    }
  ]
}
"@
Write-Json (Join-Path $Tmp "trust.json") $trust
Write-Json (Join-Path $Tmp "iam.json") $iam
$trustPath = "file://" + ((Join-Path $Tmp "trust.json") -replace "\\", "/")
$iamPath = "file://" + ((Join-Path $Tmp "iam.json") -replace "\\", "/")

Step "Deploy role $RoleName"
aws iam get-role --profile $AwsProfile --role-name $RoleName 2>$null | Out-Null
if ($LASTEXITCODE -ne 0) {
  aws iam create-role --profile $AwsProfile --role-name $RoleName --assume-role-policy-document $trustPath --max-session-duration 3600 --description "GitHub Actions deploy for $FullRepo (Tango nonprod)" | Out-Null
} else {
  aws iam update-assume-role-policy --profile $AwsProfile --role-name $RoleName --policy-document $trustPath | Out-Null
}
if ($LASTEXITCODE -ne 0) { Fail "Could not create or update the deploy role." }
aws iam attach-role-policy --profile $AwsProfile --role-name $RoleName --policy-arn arn:aws:iam::aws:policy/PowerUserAccess | Out-Null
aws iam put-role-policy --profile $AwsProfile --role-name $RoleName --policy-name tango-iam --policy-document $iamPath | Out-Null
if ($LASTEXITCODE -ne 0) { Fail "Could not attach the role policies." }
$RoleArn = "arn:aws:iam::${Account}:role/$RoleName"

Step "Repository variables"
gh variable set AWS_ROLE_ARN --repo $FullRepo --body $RoleArn
gh variable set AWS_REGION --repo $FullRepo --body $Region
if ($LASTEXITCODE -ne 0) { Fail "Could not save repository variables." }

Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue

Step "Done"
Write-Host "Repository : https://github.com/$FullRepo"
Write-Host "AWS role   : $RoleArn"
Write-Host ""
Write-Host "Opening the 'Deploy nonprod' workflow. Press 'Run workflow' to create the server" -ForegroundColor Green
Write-Host "(about USD 22-25 per month while it exists; 'Destroy nonprod' deletes it)." -ForegroundColor Green
Start-Process "https://github.com/$FullRepo/actions/workflows/deploy-nonprod.yml"
