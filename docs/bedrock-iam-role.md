# Bedrock with an IAM workload role

Choose **IAM role (temporary credentials)** when creating an Anthropic Bedrock account. Enter the target role ARN and the Bedrock source region. On ECS, the AWS SDK obtains the task role credentials automatically; the task execution role is not used for model calls. No access key or session token needs to be saved in the gateway account.

The source task role must allow `sts:AssumeRole` on the target role, and the target must trust the source role. The target role must allow inference on the selected profile and its model resources. For Global inference, enable Force Global and grant only the desired `global.*` profiles. Model access agreements and account quotas must also be available in the target account.

Credentials are shared across requests for each role, region, and external ID. STS sessions last one hour and refresh five minutes before expiration. Retrieval and signing failures fail the request rather than falling back to static credentials.

The admin API accepts the following credential fields:

```json
{
  "auth_mode": "iam_role",
  "aws_role_arn": "arn:aws:iam::123456789012:role/bedrock-inference",
  "aws_region": "us-east-1",
  "aws_force_global": "true"
}
```

`aws_external_id` is optional when the target trust policy requires one. The role credential provider uses the server's default AWS credential chain. Prefer an ECS task role; avoid static source credentials in the server environment. The existing SigV4 key and API key modes remain available.

A successful IAM policy simulation does not establish model availability. Validate a small non-sensitive request, streaming, model mapping, credential refresh, and usage reporting before routing production traffic. An AWS account-level model access denial must be resolved with AWS and cannot be fixed by broadening IAM permissions.
