package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bitrise-io/bitrise/log"
	"github.com/bitrise-io/bitrise/models"
	"github.com/bitrise-io/go-utils/command"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

type RunningContainer struct {
	ID   string
	Name string
}

func (rc *RunningContainer) Destroy() error {
	_, err := command.New("docker", "rm", "--force", rc.Name).RunAndReturnTrimmedCombinedOutput()
	if err != nil {
		// rc.logger.Errorf(out)
		return fmt.Errorf("remove docker container: %w", err)
	}
	return nil
}

func (rc *RunningContainer) ExecuteCommandArgs(envs []string) []string {
	args := []string{"exec"}

	for _, env := range envs {
		args = append(args, "-e", env)
	}

	args = append(args, rc.Name)

	return args
}

type ContainerManager struct {
	logger             log.Logger
	workflowContainers map[string]*RunningContainer
	serviceContainers  map[string][]*RunningContainer
	client             *client.Client
}

func NewContainerManager(logger log.Logger) *ContainerManager {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Warnf("Docker client failed to initialize (possibly running on unsupported stack): %s", err)
	}

	return &ContainerManager{
		logger:             logger,
		workflowContainers: make(map[string]*RunningContainer),
		serviceContainers:  make(map[string][]*RunningContainer),
		client:             dockerClient,
	}
}

func (cm *ContainerManager) Login(container models.Container, envs map[string]string) error {
	cm.logger.Infof("Running workflow in docker container: %s", container.Image)
	cm.logger.Debugf("Docker cred: %s", container.Credentials)

	if container.Credentials.Username != "" && container.Credentials.Password != "" {
		cm.logger.Debugf("Logging into docker registry: %s", container.Image)

		password := container.Credentials.Password
		if strings.HasPrefix(password, "$") {
			if value, ok := envs[strings.TrimPrefix(container.Credentials.Password, "$")]; ok {
				password = value
			}
		}

		args := []string{"login", "--username", container.Credentials.Username, "--password", password}

		if container.Credentials.Server != "" {
			args = append(args, container.Credentials.Server)
		} else if len(strings.Split(container.Image, "/")) > 2 {
			args = append(args, container.Image)
		}

		cm.logger.Debugf("Running command: docker %s", strings.Join(args, " "))

		out, err := command.New("docker", args...).RunAndReturnTrimmedCombinedOutput()
		if err != nil {
			cm.logger.Errorf(out)
			return fmt.Errorf("run docker login: %w", err)
		}
	}
	return nil
}

func (cm *ContainerManager) StartWorkflowContainer(container models.Container, workflowID string) (*RunningContainer, error) {
	containerName := fmt.Sprintf("workflow-%s", workflowID)
	dockerMountOverrides := strings.Split(os.Getenv("BITRISE_DOCKER_MOUNT_OVERRIDES"), ",")
	// TODO: make sure the sleep command works across OS flavours
	runningContainer, err := cm.startContainer(container, containerName, dockerMountOverrides, "sleep infinity", "/bitrise/src", "root")
	if err != nil {
		return runningContainer, fmt.Errorf("start workflow container: %w", err)
	}
	cm.workflowContainers[workflowID] = runningContainer

	if err := cm.healthCheckContainer(context.Background(), runningContainer); err != nil {
		return runningContainer, fmt.Errorf("container health check: %w", err)
	}

	return runningContainer, nil
}

func (cm *ContainerManager) StartServiceContainers(services map[string]models.Container, workflowID string) ([]*RunningContainer, error) {
	var containers []*RunningContainer
	for serviceName := range services {
		// Naming the container other than the service name, can cause issues with network calls
		runningContainer, err := cm.startContainer(services[serviceName], serviceName, []string{}, "", "", "")
		containers = append(containers, runningContainer)
		if err != nil {
			return containers, fmt.Errorf("start service container (%s): %w", serviceName, err)
		}
	}

	cm.serviceContainers[workflowID] = append(cm.serviceContainers[workflowID], containers...)

	for _, container := range containers {
		if err := cm.healthCheckContainer(context.Background(), container); err != nil {
			return containers, fmt.Errorf("container health check: %w", err)
		}
	}

	return containers, nil
}

func (cm *ContainerManager) GetWorkflowContainer(workflowID string) *RunningContainer {
	return cm.workflowContainers[workflowID]
}

func (cm *ContainerManager) GetServiceContainers(workflowID string) []*RunningContainer {
	return cm.serviceContainers[workflowID]
}

func (cm *ContainerManager) DestroyAllContainers() error {
	for _, container := range cm.workflowContainers {
		if err := container.Destroy(); err != nil {
			return fmt.Errorf("destroy workflow container: %w", err)
		}
	}

	for _, containers := range cm.serviceContainers {
		for _, container := range containers {
			if err := container.Destroy(); err != nil {
				return fmt.Errorf("destroy service container: %w", err)
			}
		}
	}

	return nil
}

// We are not using the docker sdk here for multiple reasons:
//   - We want to make sure the end user can easily debug using the same docker command we issue
//     (hard to convert between sdk and cli api)
//   - We'd like to support options generically,
//     with the SDK we would need to parse the string ourselves to convert them properly to their own type
func (cm *ContainerManager) startContainer(container models.Container,
	name string,
	volumes []string,
	commandArgs, workingDir, user string,
) (*RunningContainer, error) {
	if err := cm.ensureNetwork(); err != nil {
		return nil, fmt.Errorf("ensure bitrise docker network: %w", err)
	}

	dockerRunArgs := []string{"create",
		"--platform", "linux/amd64",
		"--network=bitrise",
	}

	for _, o := range volumes {
		dockerRunArgs = append(dockerRunArgs, "-v", o)
	}

	for _, env := range container.Envs {
		for name, value := range env {
			dockerRunArgs = append(dockerRunArgs, "-e", fmt.Sprintf("%s=%s", name, value))
		}
	}

	for _, port := range container.Ports {
		dockerRunArgs = append(dockerRunArgs, "-p", port)
	}

	if workingDir != "" {
		dockerRunArgs = append(dockerRunArgs, "-w", workingDir)
	}

	if user != "" {
		dockerRunArgs = append(dockerRunArgs, "-u", user)
	}

	if container.Options != "" {
		// This regex splits the string by spaces, but keeps quoted strings together
		// For example --health-cmd "redis-cli ping" will be split into: "--health-cmd", "redis-cli ping"
		r := regexp.MustCompile(`[^\s"']+|"([^"]*)"|'([^']*)`)
		result := r.FindAllString(container.Options, -1)

		// Remove quotes from the strings
		var options []string
		for _, result := range result {
			options = append(options, strings.ReplaceAll(result, "\"", ""))
		}

		dockerRunArgs = append(dockerRunArgs, options...)
	}

	dockerRunArgs = append(dockerRunArgs,
		fmt.Sprintf("--name=%s", name),
		container.Image,
	)

	// TODO: think about enabling setting this on the public UI as well
	if commandArgs != "" {
		commandArgsList := strings.Split(commandArgs, " ")
		dockerRunArgs = append(dockerRunArgs, commandArgsList...)
	}

	log.Infof("ℹ️ Running command: docker %s", strings.Join(dockerRunArgs, " "))
	out, err := command.New("docker", dockerRunArgs...).RunAndReturnTrimmedCombinedOutput()
	if err != nil {
		log.Errorf(out)
		return nil, fmt.Errorf("create docker container (%s): %w", name, err)
	}

	runningContainer := &RunningContainer{
		Name: name,
	}

	log.Infof("ℹ️ Running command: docker start %s", name)
	out, err = command.New("docker", "start", name).RunAndReturnTrimmedCombinedOutput()
	if err != nil {
		log.Errorf(out)
		return runningContainer, fmt.Errorf("start docker container (%s): %w", name, err)
	}

	result, err := cm.getRunningContainer(context.Background(), name)
	if result != nil {
		runningContainer.ID = result.ID
	}
	if err != nil {
		return runningContainer, fmt.Errorf("container (%s) unable to start properly: %w", name, err)
	}

	log.Infof("✅ Container (%s) is running", name)

	return runningContainer, nil
}

func (cm *ContainerManager) getRunningContainer(ctx context.Context, name string) (*types.Container, error) {
	containers, err := cm.client.ContainerList(ctx, types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	if len(containers) != 1 {
		return nil, fmt.Errorf("multiple containers with the same name found: %s", name)
	}

	if containers[0].State != "running" {
		logs, err := cm.client.ContainerLogs(ctx, containers[0].ID, types.ContainerLogsOptions{
			ShowStdout: true,
			ShowStderr: true,
		})
		if err != nil {
			return &containers[0], fmt.Errorf("container is not running: failed to get container logs: %w", err)
		}

		content, err := io.ReadAll(logs)
		if err != nil {
			return &containers[0], fmt.Errorf("container is not running: failed to read container logs: %w", err)
		}
		cm.logger.Errorf("Failed container (%s) logs:\n %s\n", name, string(content))
		return &containers[0], fmt.Errorf("container (%s) is not running", name)
	}
	return &containers[0], nil

}

func (cm *ContainerManager) healthCheckContainer(ctx context.Context, container *RunningContainer) error {
	inspect, err := cm.client.ContainerInspect(ctx, container.ID)
	if err != nil {
		return fmt.Errorf("inspect container (%s): %w", container.Name, err)
	}

	if inspect.State.Health == nil {
		cm.logger.Infof("✅ No healthcheck is defined for container (%s), assuming healthy...", container.Name)
		return nil
	}

	retries := 0
	for inspect.State.Health.Status != "healthy" {
		if inspect.State.Health.Status == "unhealthy" {
			cm.logger.Errorf("❌ Container (%s) is unhealthy...", container.Name)
			return fmt.Errorf("container (%s) is unhealthy", container.Name)
		}
		// this solution prefers quick retries at the beginning and constant for the rest
		sleep := 5
		if retries < 5 {
			sleep = retries
		}
		time.Sleep(time.Duration(sleep) * time.Second)

		cm.logger.Infof("⏳ Waiting for container (%s) to be healthy... (retry: %ds)", container.Name, sleep)
		inspect, err = cm.client.ContainerInspect(context.Background(), container.ID)
		if err != nil {
			return fmt.Errorf("inspect container (%s): %w", container.Name, err)
		}
		retries++

	}

	cm.logger.Infof("✅ Container (%s) is healthy...", container.Name)
	return nil
}

func (cm *ContainerManager) ensureNetwork() error {
	networks, err := cm.client.NetworkList(context.Background(), types.NetworkListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "bitrise")),
	})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}

	if len(networks) > 0 {
		return nil
	}

	if _, err := cm.client.NetworkCreate(context.Background(), "bitrise", types.NetworkCreate{}); err != nil {
		return fmt.Errorf("create network: %w", err)
	}

	return nil
}
