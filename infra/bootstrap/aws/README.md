# AWS bootstrap

Shell scripts that set up the AWS account the demo environment runs in
([design section 13](../../../docs/design.md#13-demo-environment-aws)). The owner runs them once,
by hand; afterwards `infra/demo/` (OpenTofu) builds and destroys everything inside the account.
Adapted from cferrante-dev's bootstrap, whose AWS Organization and Identity Center sign-in this
reuses.

**Why scripts and not OpenTofu.** OpenTofu needs an account and a sign-in before it can run, and
account-level resources are risky to delete by accident, so they stay out of any state that
`tofu destroy` can act on.

## What you end up with

```
Management account (existing)            Project account "call-event-pipeline" (new)
├─ AWS Organization                     ├─ Your Identity Center user: AdministratorAccess
├─ IAM Identity Center (your sign-in)   ├─ Budget: $10/month, alerts at 50% and 100% actual,
└─ cferrante-dev's account              │  100% forecast
                                        ├─ Groq API key (SSM SecureString)
                                        └─ The demo environment, only while it runs
```

*Why a separate account:* a clean cost signal, a small blast radius, and teardown by closing it.

## Assumptions

- **AWS CLI v2** and **bash**; nothing else (no `jq`).
- The management account already has an Organization and IAM Identity Center, as set up by
  cferrante-dev's bootstrap, and you have a CLI profile for it.
- The Organization may create another account (new organizations get a small quota at first).

## Setup

```bash
cd infra/bootstrap/aws
cp config.env.example config.env     # git-ignored; fill in the blanks
aws sso login --profile <MGMT_PROFILE>
./check.sh                           # read-only: ✓ / ✗ per step; exits 1 until all are done
```

## Steps

Run in order. Each checks what exists and creates only what's missing, so re-running is safe.

| Script | Does | Account | Cost |
|---|---|---|---|
| `check.sh` | Reports which steps are done; exits non-zero while any is not. Changes nothing | both | $0 |
| `prices.sh` | Looks up the demo instances' on-demand prices in AWS's Price List API and prints a session's cost. Changes nothing | management | $0 |
| `01-account.sh` | Creates the project account (waits up to 10 min) | management | $0 |
| `02-sso-access.sh` | Grants your user the permission set in the project account; writes the `PROJECT_PROFILE` CLI profile | management | $0 |
| `03-budget.sh` | Monthly $10 cost budget, email alerts at 50% and 100% actual, 100% forecast | project | $0 (the first two budgets are free) |
| `04-groq-key.sh` | Stores the Groq API key as an encrypted SSM SecureString after checking it with Groq. Re-running offers to replace it | project | $0 (standard tier) |

After `02-sso-access.sh`, sign in to the new profile once: `aws sso login --profile <PROJECT_PROFILE>`.

Tests that run offline, with nothing sent to AWS: `./check_test.sh` and `./secret_test.sh`.

## Costs

Everything here is free. The money is spent by the demo environment, only while it runs; see
`prices.sh` for a session's cost. Charges appear on the management account's bill
(consolidated billing). Member accounts get no free tier of their own.

**The budget can't stop spending:** its data updates a few times a day. The guard is that every
demo instance terminates itself after 3 hours.

## Troubleshooting

| Message | Fix |
|---|---|
| `no valid session for profile …` | `aws sso login --profile …` (sessions expire after a few hours) |
| `Missing …/config.env` | `cp config.env.example config.env` and fill it in |
| `No access` when signing in to the project profile | The new assignment takes a minute to apply; retry |
| `account creation failed: EMAIL_ALREADY_EXISTS` | `PROJECT_ACCOUNT_EMAIL` belongs to another AWS account; use a different plus address |
| `account creation failed: ACCOUNT_NUMBER_LIMIT_EXCEEDED` | The Organization's account quota is reached: request an increase in Service Quotas, or close an unused account |
| `still in progress …; re-run this script later` | Re-run it: the script resumes rather than starting again |
| `Groq didn't accept the key (HTTP 401)` | The key is wrong or revoked; create one in the Groq console. Nothing was stored |

## Undo

Nothing here deletes resources; do it in the console, in reverse order:

1. **Groq key:** `aws ssm delete-parameter --name /call-event-pipeline/groq-api-key --profile <PROJECT_PROFILE>`,
   and revoke the key in the Groq console if it isn't shared with cferrante-dev.
2. **Budget:** Billing → Budgets in the project account.
3. **Access:** IAM Identity Center → AWS accounts → remove the assignment; delete the profile
   from `~/.aws/config`.
4. **Account:** Organizations → close the account. Closed accounts can be reopened for 90 days.
