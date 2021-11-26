# AWS ASG Node Detach

Complimentary tool for the [aws-node-termination-handler](https://github.com/aws/aws-node-termination-handler)

Detaches EC2 instances from their Auto Scaling Groups when a spot interruption event is received.  
This reduces the time for a replacement instances to be provisioned significantly.

## Requirements

[Kubernetes events](https://github.com/aws/aws-node-termination-handler/blob/main/docs/kubernetes_events.md) must be enabled in the NTH.

NTH must be running in IMDS mode because we rely on the instance metadata being available in the event.  
This will **not** work in Queue Processor mode!

## IAM

Requires permissions to describe instances and detach instances from ASG

```
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "autoscaling:DetachInstances"
      ],
      "Resource": "*"
    }
  ]
}
```
