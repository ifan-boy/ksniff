package kube

import (
	"fmt"
	"io"
	"ksniff/utils"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type KubernetesApiService interface {
	ExecuteCommand(podName string, containerName string, command []string, stdOut io.Writer) (int, error)

	DeletePod(podName string) error

	CreatePrivilegedPod(nodeName string) (*corev1.Pod, error)

	UploadFile(localPath string, remotePath string, podName string, containerName string) error
}

type KubernetesApiServiceImpl struct {
	clientset       *kubernetes.Clientset
	restConfig      *rest.Config
	targetNamespace string
}

func NewKubernetesApiService(clientset *kubernetes.Clientset,
	restConfig *rest.Config, targetNamespace string) KubernetesApiService {

	return &KubernetesApiServiceImpl{clientset: clientset,
		restConfig:      restConfig,
		targetNamespace: targetNamespace}
}

func (k *KubernetesApiServiceImpl) IsDockerContainerRuntime(nodeName string) (bool, error) {
	node, err := k.clientset.CoreV1().Nodes().Get(nodeName, v1.GetOptions{})
	if err != nil {
		return false, err
	}

	nodeRuntimeVersion := node.Status.NodeInfo.ContainerRuntimeVersion

	if strings.TrimPrefix(nodeRuntimeVersion, "docker") == nodeRuntimeVersion {
		return false, nil
	}

	return true, nil
}

func (k *KubernetesApiServiceImpl) ExecuteCommand(podName string, containerName string, command []string, stdOut io.Writer) (int, error) {

	log.Infof("executing command: '%s' on container: '%s', pod: '%s', namespace: '%s'", command, containerName, podName, k.targetNamespace)
	stdErr := new(Writer)

	executeTcpdumpRequest := ExecCommandRequest{
		KubeRequest: KubeRequest{
			Clientset:  k.clientset,
			RestConfig: k.restConfig,
			Namespace:  k.targetNamespace,
			Pod:        podName,
			Container:  containerName,
		},
		Command: command,
		StdErr:  stdErr,
		StdOut:  stdOut,
	}

	exitCode, err := PodExecuteCommand(executeTcpdumpRequest)
	if err != nil {
		log.WithError(err).Error("failed executing command: '%s', exitCode: '%d', stdErr: '%s'",
			command, exitCode, stdErr.Output)

		return exitCode, err
	}

	log.Infof("command: '%s' executing successfully exitCode: '%d', stdErr :'%s'", command, exitCode, stdErr.Output)

	return exitCode, err
}

func (k *KubernetesApiServiceImpl) DeletePod(podName string) error {

	log.Infof("removing privileged pod: '%s'", podName)
	defer log.Infof("privileged pod: '%s' removed", podName)

	var gracePeriodTime int64 = 0

	err := k.clientset.CoreV1().Pods(k.targetNamespace).Delete(podName, &v1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodTime,
	})

	return err
}

func (k *KubernetesApiServiceImpl) CreatePrivilegedPod(nodeName string) (*corev1.Pod, error) {
	log.Debugf("creating privileged pod on remote node")

	isDockerRuntime, err := k.IsDockerContainerRuntime(nodeName)
	if err != nil {
		return nil, err
	}

	if !isDockerRuntime {
		return nil, errors.Errorf("container runtime on node: '%s' isn't docker", nodeName)
	}

	typeMetadata := v1.TypeMeta{
		Kind:       "Pod",
		APIVersion: "v1",
	}

	objectMetadata := v1.ObjectMeta{
		GenerateName: "ksniff-",
		Namespace:    k.targetNamespace,
	}

	volumeMounts := []corev1.VolumeMount{{
		Name:      "docker-sock",
		ReadOnly:  true,
		MountPath: "/var/run/docker.sock",
	}}

	privileged := true
	privilegedContainer := corev1.Container{
		Name:  "ksniff-privileged",
		Image: "docker",
                ImagePullPolicy: corev1.PullIfNotPresent,

		SecurityContext: &corev1.SecurityContext{
			Privileged: &privileged,
		},

		Command:      []string{"sh", "-c", "sleep 10000000"},
		VolumeMounts: volumeMounts,
	}

	hostPathType := corev1.HostPathFile
	volumeSources := corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{
			Path: "/var/run/docker.sock",
			Type: &hostPathType,
		},
	}

	podSpecs := corev1.PodSpec{
		NodeName:      nodeName,
		RestartPolicy: corev1.RestartPolicyNever,
		Containers:    []corev1.Container{privilegedContainer},
		Volumes: []corev1.Volume{{
			Name:         "docker-sock",
			VolumeSource: volumeSources,
		},
		},
	}

	pod := corev1.Pod{
		TypeMeta:   typeMetadata,
		ObjectMeta: objectMetadata,
		Spec:       podSpecs,
	}

	createdPod, err := k.clientset.CoreV1().Pods(k.targetNamespace).Create(&pod)
	if err != nil {
		return nil, err
	}

	log.Infof("pod created: %v", createdPod)

	verifyPodState := func() bool {
		podStatus, err := k.clientset.CoreV1().Pods(k.targetNamespace).Get(createdPod.Name, v1.GetOptions{})
		if err != nil {
			return false
		}

		if podStatus.Status.Phase == corev1.PodRunning {
			return true
		}

		return false
	}

	log.Info("waiting for pod successful startup")

	podCreationTimeout := 1 * time.Minute
	if !utils.RunWhileFalse(verifyPodState, podCreationTimeout, 1*time.Second) {
		return nil, errors.Errorf("failed to create pod within timeout (%s)", podCreationTimeout)
	}

	return createdPod, nil
}

func (k *KubernetesApiServiceImpl) checkIfFileExistOnPod(remotePath string, podName string, containerName string) (bool, error) {
	stdOut := new(Writer)
	stdErr := new(Writer)

	command := []string{"/bin/sh", "-c", fmt.Sprintf("ls -alt %s", remotePath)}

	exitCode, err := k.ExecuteCommand(podName, containerName, command, stdOut)
	if err != nil {
		return false, err
	}

	if exitCode != 0 {
		return false, nil
	}

	if stdErr.Output != "" {
		return false, errors.New("failed to check for tcpdump")
	}

	log.Infof("file found: '%s'", stdOut.Output)

	return true, nil
}

func (k *KubernetesApiServiceImpl) UploadFile(localPath string, remotePath string, podName string, containerName string) error {
	log.Infof("uploading file: '%s' to '%s' on container: '%s'", localPath, remotePath, containerName)

	isExist, err := k.checkIfFileExistOnPod(remotePath, podName, containerName)
	if err != nil {
		return err
	}

	if isExist {
		log.Info("file was already found on remote pod")
		return nil
	}

	log.Infof("file not found on: '%s', starting to upload", remotePath)

	req := UploadFileRequest{
		KubeRequest: KubeRequest{
			Clientset:  k.clientset,
			RestConfig: k.restConfig,
			Namespace:  k.targetNamespace,
			Pod:        podName,
			Container:  containerName,
		},
		Src: localPath,
		Dst: remotePath,
	}

	exitCode, err := PodUploadFile(req)
	if err != nil || exitCode != 0 {
		return errors.Wrapf(err, "upload file failed, exitCode: %d", exitCode)
	}

	log.Info("verifying file uploaded successfully")

	isExist, err = k.checkIfFileExistOnPod(remotePath, podName, containerName)
	if err != nil {
		return err
	}

	if !isExist {
		log.Error("failed to upload file.")
		return errors.New("couldn't locate file on pod after upload done")
	}

	log.Info("file uploaded successfully")

	return nil
}
