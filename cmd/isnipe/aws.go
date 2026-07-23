package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
)

// loadConfig resolves credentials through the standard SDK chain (env vars,
// shared config/credentials files, SSO cache, IMDS), honoring an explicit
// profile/region when given.
func loadConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// callerIdentity returns the caller's ARN, proving the credentials work.
func callerIdentity(ctx context.Context, cfg aws.Config) (string, error) {
	c := cfg
	if c.Region == "" {
		c.Region = "us-east-1" // STS needs a region even though the call is global
	}
	resp, err := sts.NewFromConfig(c).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(resp.Arn), nil
}

// apiErrorCode extracts the AWS error code (e.g. "InsufficientInstanceCapacity")
// from an SDK error, or "" if it isn't an API error.
func apiErrorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

type stoppedInstance struct {
	ID, Type, AZ, Name string
}

func describeStopped(ctx context.Context, c *ec2.Client) ([]stoppedInstance, error) {
	var res []stoppedInstance
	p := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []string{"stopped"},
		}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				name := ""
				for _, t := range inst.Tags {
					if aws.ToString(t.Key) == "Name" {
						name = aws.ToString(t.Value)
					}
				}
				az := ""
				if inst.Placement != nil {
					az = aws.ToString(inst.Placement.AvailabilityZone)
				}
				res = append(res, stoppedInstance{
					ID:   aws.ToString(inst.InstanceId),
					Type: string(inst.InstanceType),
					AZ:   az,
					Name: name,
				})
			}
		}
	}
	return res, nil
}

func startInstance(ctx context.Context, c *ec2.Client, id string) error {
	_, err := c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}})
	return err
}

func instanceState(ctx context.Context, c *ec2.Client, id string) (string, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return "", err
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.State != nil {
				return string(inst.State.Name), nil
			}
		}
	}
	return "", fmt.Errorf("instance %s not found", id)
}

func waitRunning(ctx context.Context, c *ec2.Client, id string, timeout time.Duration) error {
	w := ec2.NewInstanceRunningWaiter(c)
	return w.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, timeout)
}

func publicAddr(ctx context.Context, c *ec2.Client, id string) (ip, dns string) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return "", ""
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			return aws.ToString(inst.PublicIpAddress), aws.ToString(inst.PublicDnsName)
		}
	}
	return "", ""
}

// listProfiles reads profile names from the shared AWS config/credentials files.
func listProfiles() []string {
	seen := map[string]bool{}
	var res []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			res = append(res, p)
		}
	}
	home, _ := os.UserHomeDir()

	cfgFile := os.Getenv("AWS_CONFIG_FILE")
	if cfgFile == "" {
		cfgFile = filepath.Join(home, ".aws", "config")
	}
	credFile := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credFile == "" {
		credFile = filepath.Join(home, ".aws", "credentials")
	}

	for _, line := range readLines(cfgFile) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			// config sections are "[profile foo]" and "[default]"; skip
			// "[sso-session ...]" / "[services ...]" helper sections.
			if strings.HasPrefix(name, "sso-session ") || strings.HasPrefix(name, "services ") {
				continue
			}
			add(strings.TrimPrefix(name, "profile "))
		}
	}
	for _, line := range readLines(credFile) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			add(strings.TrimSpace(line[1 : len(line)-1]))
		}
	}
	sort.Strings(res)
	return res
}
