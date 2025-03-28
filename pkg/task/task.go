/*
Package task provides simple functions to run task on ECS.

Usage:

	import "github.com/h3poteto/ecs-task/pkg/task"

# Run a task

When you want to run a task on ECS, please use this package as follows.

At first, you have to get a task definition. The task definition is used to run a task.

For example:

	t, err := task.NewTask("cluster-name", "container-name", "task-definition-arn or family", "commands", false, "", 300 * time.Second, "profile", "region", "task-size-cpu", "task-size-memory")

	// At first you have to get a task definition.
	taskDef, err := t.taskDefinition.DescribeTaskDefinition(t.TaskDefinitionName)
	if err != nil {
	    return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), t.Timeout)
	defer cancel()

	// Call run task API.
	tasks, err := t.RunTask(ctx, taskDef)
	if err != nil {
	    return err
	}

	// And wait to completion of task execution.
	err = t.WaitTask(ctx, tasks)

# Polling CloudWatch Logs

You can polling CloudWatch Logs log stream.

For example:

	// Get log group.
	group, streamPrefix, err := t.taskDefinition.GetLogGroup(taskDef, "Container Name")
	if err != nil {
	    return err
	}

	w := NewWatcher(group, streamPrefix+"/" + "Container Name" + "Task ID", "AWS profile name", "ap-northeast-1")
	err = w.Polling(ctx)
	if err != nil {
	    return err
	}
*/
package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	shellwords "github.com/mattn/go-shellwords"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type ECSClient interface {
	RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
}

// Task has target ECS information, client of aws-sdk-go, command and timeout seconds.
type Task struct {
	awsECS  ECSClient
	awsLogs *cloudwatchlogs.Client

	// ECS Cluster where you want to run the task.
	Cluster string
	// Container name which you want to run. Sometimes Task Definition has some container. So this package have to determine the container for run task.
	Container string
	// Name of Task Definition. You can provide full ARN, family or family:revision.
	TaskDefinitionName string
	taskDefinition     *TaskDefinition
	// Command which you want to run.
	Command []string
	// If you set 0, timeout is ignored.
	Timeout time.Duration
	// EC2 or Fargate
	LaunchType ecstypes.LaunchType
	// If you set Fargate as launch type, you have to set your subnet IDs.
	// Because Fargate demands awsvpc as network configuration, so subnet IDs are required.
	Subnets []string
	// If you want to attach the security groups to ENI of the task, please set this.
	SecurityGroups []string
	// If you set Fargate as launch type, you have to set your Platform Version.
	PlatformVersion string
	// If you don't enable this flag, the task access the internet throguth NAT gateway.
	// Please read more information: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-networking.html
	AssignPublicIP  ecstypes.AssignPublicIp
	profile         string
	region          string
	timestampFormat string
	// If you wat to override CPU and Memory, please set these values.
	taskSizeCpu     string
	taskSizeMemory  string
}

// NewTask returns a new Task struct, and initialize aws ecs API client.
// If you want to run the task as Fargate, please provide fargate flag to true, and your subnet IDs for awsvpc.
// If you don't want to run the task as Fargate, please provide empty string for subnetIDs.
func NewTask(cluster, container, taskDefinitionName, command string, fargate bool, subnetIDs, securityGroupIDs, platformVersion string, timeout time.Duration, timestampFormat, profile, region, taskSizeCpu, taskSizeMemory string) (*Task, error) {
	if cluster == "" {
		return nil, errors.New("Cluster name is required")
	}
	if container == "" {
		return nil, errors.New("Container name is required")
	}
	if taskDefinitionName == "" {
		return nil, errors.New("Task definition is required")
	}
	if command == "" {
		return nil, errors.New("Command is required")
	}
	cfg, err := newConfig(profile, region)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create AWS Session")
	}
	awsECS := ecs.NewFromConfig(cfg)
	awsLogs := cloudwatchlogs.NewFromConfig(cfg)

	taskDefinition := NewTaskDefinition(awsECS)
	p := shellwords.NewParser()
	commands, err := p.Parse(command)
	if err != nil {
		return nil, errors.Wrap(err, "Parse error")
	}
	launchType := ecstypes.LaunchTypeEc2
	assignPublicIP := ecstypes.AssignPublicIpDisabled
	if fargate {
		launchType = ecstypes.LaunchTypeFargate
		assignPublicIP = ecstypes.AssignPublicIpEnabled
	}
	subnets := []string{}
	for _, s := range strings.Split(subnetIDs, ",") {
		if len(s) > 0 {
			subnets = append(subnets, s)
		}
	}
	securityGroups := []string{}
	for _, g := range strings.Split(securityGroupIDs, ",") {
		if len(g) > 0 {
			securityGroups = append(securityGroups, g)
		}
	}

	return &Task{
		awsECS:             awsECS,
		awsLogs:            awsLogs,
		Cluster:            cluster,
		Container:          container,
		TaskDefinitionName: taskDefinitionName,
		taskDefinition:     taskDefinition,
		Command:            commands,
		Timeout:            timeout,
		LaunchType:         launchType,
		Subnets:            subnets,
		SecurityGroups:     securityGroups,
		AssignPublicIP:     assignPublicIP,
		profile:            profile,
		region:             region,
		timestampFormat:    timestampFormat,
		PlatformVersion:    platformVersion,
		taskSizeCpu:        taskSizeCpu,
		taskSizeMemory:     taskSizeMemory,
	}, nil
}

// RunTask calls run-task API. This function does not wait to completion of the task.
func (t *Task) RunTask(ctx context.Context, taskDefinition *ecstypes.TaskDefinition) (*ecstypes.Task, error) {
	containerOverride := ecstypes.ContainerOverride{
		Command: t.Command,
		Name:    aws.String(t.Container),
	}

	override := &ecstypes.TaskOverride{
		ContainerOverrides: []ecstypes.ContainerOverride{
			containerOverride,
		},
	}

	if len(t.taskSizeCpu) > 0 && len(t.taskSizeMemory) > 0 {
		override.Cpu = aws.String(t.taskSizeCpu)
		override.Memory = aws.String(t.taskSizeMemory)
	}

	var params *ecs.RunTaskInput
	if len(t.Subnets) > 0 {
		vpcConfiguration := &ecstypes.AwsVpcConfiguration{
			AssignPublicIp: t.AssignPublicIP,
			Subnets:        t.Subnets,
			SecurityGroups: t.SecurityGroups,
		}
		network := &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: vpcConfiguration,
		}
		if len(t.PlatformVersion) > 0 {
			params = &ecs.RunTaskInput{
				Cluster:              aws.String(t.Cluster),
				TaskDefinition:       taskDefinition.TaskDefinitionArn,
				Overrides:            override,
				NetworkConfiguration: network,
				LaunchType:           t.LaunchType,
				PlatformVersion:      aws.String(t.PlatformVersion),
			}
		} else {
			params = &ecs.RunTaskInput{
				Cluster:              aws.String(t.Cluster),
				TaskDefinition:       taskDefinition.TaskDefinitionArn,
				Overrides:            override,
				NetworkConfiguration: network,
				LaunchType:           t.LaunchType,
			}
		}
	} else {
		params = &ecs.RunTaskInput{
			Cluster:        aws.String(t.Cluster),
			TaskDefinition: taskDefinition.TaskDefinitionArn,
			Overrides:      override,
			LaunchType:     t.LaunchType,
		}
	}

	resp, err := t.awsECS.RunTask(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(resp.Failures) > 0 {
		log.Errorf("Run task error: %+v", resp.Failures)
		return nil, errors.New(*resp.Failures[0].Reason)
	}
	if len(resp.Tasks) == 1 {
		log.Infof("Running tasks: %+v", resp.Tasks[0])
		return &resp.Tasks[0], nil
	} else {
		return nil, errors.New(fmt.Sprintf("Expected ecs.RunTask with Count=nil to return exactly 1 task; received %d (%+v)", len(resp.Tasks), resp.Tasks))
	}
}

// WaitTask waits completion of the task execition. If timeout occures, the function exits.
func (t *Task) WaitTask(ctx context.Context, task *ecstypes.Task) error {
	log.Info("Waiting for running task...")
	err := t.waitExitTasks(ctx, *task.TaskArn)
	if err == context.DeadlineExceeded {
		err = errors.New("process timeout")
	}
	if err == nil {
		log.Info("Run task is success")
	}
	return err
}

func (t *Task) waitExitTasks(ctx context.Context, taskArn string) error {
retry:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}

		params := &ecs.DescribeTasksInput{
			Cluster: aws.String(t.Cluster),
			Tasks:   []string{taskArn},
		}
		resp, err := t.awsECS.DescribeTasks(ctx, params)
		if err != nil {
			return err
		}

		for _, task := range resp.Tasks {
			if !t.checkTaskStopped(task) {
				continue retry
			}
		}

		for _, task := range resp.Tasks {
			code, result, err := t.checkTaskSucceeded(task)
			if err != nil {
				continue retry
			}
			if !result {
				return errors.Errorf("exit code: %v", code)
			}
		}
		return nil
	}
}

func (t *Task) checkTaskStopped(task ecstypes.Task) bool {
	if *task.LastStatus != "STOPPED" {
		return false
	}
	return true
}

func (t *Task) checkTaskSucceeded(task ecstypes.Task) (int32, bool, error) {
	var targetContainer *ecstypes.Container
	for _, c := range task.Containers {
		if *c.Name == t.Container {
			targetContainer = &c
		}
	}
	if targetContainer == nil {
		return int32(1), false, errors.New("can not find target container")
	}

	if targetContainer.ExitCode == nil {
		return int32(1), false, errors.New("can not read exit code")
	}
	if *targetContainer.ExitCode != int32(0) {
		return *targetContainer.ExitCode, false, nil
	}
	return int32(0), true, nil
}
