/*
Copyright IBM Corporation 2020

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package compose

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/cli/cli/compose/loader"
	"github.com/docker/cli/cli/compose/types"
	libcomposeyaml "github.com/docker/libcompose/yaml"
	"github.com/google/go-cmp/cmp"
	"github.com/konveyor/move2kube/internal/common"
	"github.com/konveyor/move2kube/internal/containerizer"
	irtypes "github.com/konveyor/move2kube/internal/types"
	plantypes "github.com/konveyor/move2kube/types/plan"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cast"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// V3Loader loads a v3 compose file
type V3Loader struct {
}

func removeNonExistentEnvFilesV3(path string, parsedComposeFile map[string]interface{}) map[string]interface{} {
	// Remove unresolvable env files, so that the parser does not throw error
	composeFileDir := filepath.Dir(path)
	if val, ok := parsedComposeFile["services"]; ok {
		if services, ok := val.(map[string]interface{}); ok {
			for serviceName, val := range services {
				if vals, ok := val.(map[string]interface{}); ok {
					if envfilesvals, ok := vals[envFile]; ok {
						// env_file can be a string or list of strings
						// https://docs.docker.com/compose/compose-file/#env_file
						if envfilesstr, ok := envfilesvals.(string); ok {
							envFilePath := envfilesstr
							if !filepath.IsAbs(envFilePath) {
								envFilePath = filepath.Join(composeFileDir, envFilePath)
							}
							finfo, err := os.Stat(envFilePath)
							if os.IsNotExist(err) || finfo.IsDir() {
								log.Warnf("Unable to find env config file %s referred in service %s in file %s. Ignoring it.", envFilePath, serviceName, path)
								delete(vals, envFile)
							}
						} else if envfilesvalsint, ok := envfilesvals.([]interface{}); ok {
							envfiles := []interface{}{}
							for _, envfilesval := range envfilesvalsint {
								if envfilesstr, ok := envfilesval.(string); ok {
									envFilePath := envfilesstr
									if !filepath.IsAbs(envFilePath) {
										envFilePath = filepath.Join(composeFileDir, envFilePath)
									}
									finfo, err := os.Stat(envFilePath)
									if os.IsNotExist(err) || finfo.IsDir() {
										log.Warnf("Unable to find env config file %s referred in service %s in file %s. Ignoring it.", envFilePath, serviceName, path)
										continue
									}
									envfiles = append(envfiles, envfilesstr)
								}
							}
							vals[envFile] = envfiles
						}
					}
				}
			}
		}
	}
	return parsedComposeFile
}

// ParseV3 parses version 3 compose files
func ParseV3(path string) (*types.Config, error) {
	fileData, err := ioutil.ReadFile(path)
	if err != nil {
		log.Errorf("Unable to load Compose file at path %s Error: %q", path, err)
		return nil, err
	}
	// Parse the Compose File
	parsedComposeFile, err := loader.ParseYAML(fileData)
	if err != nil {
		log.Errorf("Unable to load Compose file at path %s Error: %q", path, err)
		return nil, err
	}
	parsedComposeFile = removeNonExistentEnvFilesV3(path, parsedComposeFile)
	// Config details
	configDetails := types.ConfigDetails{
		WorkingDir:  filepath.Dir(path),
		ConfigFiles: []types.ConfigFile{{Filename: path, Config: parsedComposeFile}},
		Environment: getEnvironmentVariables(),
	}
	config, err := loader.Load(configDetails)
	if err != nil {
		log.Errorf("Unable to load Compose file at path %s Error: %q", path, err)
		return nil, err
	}
	return config, nil
}

// ConvertToIR loads an v3 compose file into IR
func (c *V3Loader) ConvertToIR(composefilepath string, plan plantypes.Plan, service plantypes.Service) (irtypes.IR, error) {
	log.Debugf("About to load configuration from docker compose file at path %s", composefilepath)
	config, err := ParseV3(composefilepath)
	if err != nil {
		log.Warnf("Error while loading docker compose config : %s", err)
		return irtypes.IR{}, err
	}
	log.Debugf("About to start loading docker compose to intermediate rep")
	return c.convertToIR(filepath.Dir(composefilepath), *config, plan, service)
}

func (c *V3Loader) convertToIR(filedir string, composeObject types.Config, plan plantypes.Plan, service plantypes.Service) (irtypes.IR, error) {
	ir := irtypes.IR{
		Services: map[string]irtypes.Service{},
	}

	//Secret volumes translated to IR
	ir.Storages = c.getSecretStorages(composeObject.Secrets)

	//ConfigMap volumes translated to IR
	ir.Storages = append(ir.Storages, c.getConfigStorages(composeObject.Configs)...)

	for _, composeServiceConfig := range composeObject.Services {
		if composeServiceConfig.Name != service.ServiceName {
			continue
		}
		name := common.NormalizeForServiceName(composeServiceConfig.Name)
		serviceConfig := irtypes.NewServiceWithName(name)
		serviceContainer := corev1.Container{}

		serviceContainer.Image = composeServiceConfig.Image
		if serviceContainer.Image == "" {
			serviceContainer.Image = name + ":latest"
		}
		if composeServiceConfig.Build.Dockerfile != "" || composeServiceConfig.Build.Context != "" {
			//TODO: Add support for args and labels
			// filedir, name, serviceContainer.Image, composeServiceConfig.Build.Dockerfile, composeServiceConfig.Build.Context

			con, err := new(containerizer.ReuseDockerfileContainerizer).GetContainer(plan, service)
			if err != nil {
				log.Warnf("Unable to get containization script even though build parameters are present : %s", err)
			} else {
				ir.AddContainer(con)
			}
		}
		serviceContainer.WorkingDir = composeServiceConfig.WorkingDir
		serviceContainer.Command = composeServiceConfig.Entrypoint
		serviceContainer.Args = composeServiceConfig.Command
		serviceContainer.Stdin = composeServiceConfig.StdinOpen
		serviceContainer.Name = strings.ToLower(composeServiceConfig.ContainerName)
		if serviceContainer.Name == "" {
			serviceContainer.Name = strings.ToLower(serviceConfig.Name)
		}
		serviceContainer.TTY = composeServiceConfig.Tty
		serviceContainer.Ports = c.getPorts(composeServiceConfig.Ports, composeServiceConfig.Expose)
		c.addPorts(composeServiceConfig.Ports, composeServiceConfig.Expose, &serviceConfig)

		serviceConfig.Annotations = map[string]string(composeServiceConfig.Labels)
		serviceConfig.Labels = common.MergeStringMaps(composeServiceConfig.Labels, composeServiceConfig.Deploy.Labels)
		if composeServiceConfig.Hostname != "" {
			serviceConfig.Hostname = composeServiceConfig.Hostname
		}
		if composeServiceConfig.DomainName != "" {
			serviceConfig.Subdomain = composeServiceConfig.DomainName
		}
		if composeServiceConfig.Pid != "" {
			if composeServiceConfig.Pid == "host" {
				serviceConfig.HostPID = true
			} else {
				log.Warnf("Ignoring PID key for service \"%v\". Invalid value \"%v\".", name, composeServiceConfig.Pid)
			}
		}
		securityContext := &corev1.SecurityContext{}
		if composeServiceConfig.Privileged {
			securityContext.Privileged = &composeServiceConfig.Privileged
		}
		if composeServiceConfig.User != "" {
			uid, err := strconv.ParseInt(composeServiceConfig.User, 10, 64)
			if err != nil {
				log.Warn("Ignoring user directive. User to be specified as a UID (numeric).")
			} else {
				securityContext.RunAsUser = &uid
			}
		}
		capsAdd := []corev1.Capability{}
		capsDrop := []corev1.Capability{}
		for _, capAdd := range composeServiceConfig.CapAdd {
			capsAdd = append(capsAdd, corev1.Capability(capAdd))
		}
		for _, capDrop := range composeServiceConfig.CapDrop {
			capsDrop = append(capsDrop, corev1.Capability(capDrop))
		}
		//set capabilities if it is not empty
		if len(capsAdd) > 0 || len(capsDrop) > 0 {
			securityContext.Capabilities = &corev1.Capabilities{
				Add:  capsAdd,
				Drop: capsDrop,
			}
		}
		// update template only if securityContext is not empty
		if *securityContext != (corev1.SecurityContext{}) {
			serviceContainer.SecurityContext = securityContext
		}
		podSecurityContext := &corev1.PodSecurityContext{}
		if !cmp.Equal(*podSecurityContext, corev1.PodSecurityContext{}) {
			serviceConfig.SecurityContext = podSecurityContext
		}

		if composeServiceConfig.Deploy.Mode == "global" {
			serviceConfig.Daemon = true
		}

		serviceConfig.Networks = c.getNetworks(composeServiceConfig, composeObject)

		if (composeServiceConfig.Deploy.Resources != types.Resources{}) {
			if composeServiceConfig.Deploy.Resources.Limits != nil {
				resourceLimit := corev1.ResourceList{}
				memLimit := libcomposeyaml.MemStringorInt(composeServiceConfig.Deploy.Resources.Limits.MemoryBytes)
				if memLimit != 0 {
					resourceLimit[corev1.ResourceMemory] = *resource.NewQuantity(int64(memLimit), "RandomStringForFormat")
				}
				if composeServiceConfig.Deploy.Resources.Limits.NanoCPUs != "" {
					cpuLimit, err := strconv.ParseFloat(composeServiceConfig.Deploy.Resources.Limits.NanoCPUs, 64)
					if err != nil {
						log.Warnf("Unable to convert cpu limits resources value : %s", err)
					}
					CPULimit := int64(cpuLimit * 1000)
					if CPULimit != 0 {
						resourceLimit[corev1.ResourceCPU] = *resource.NewMilliQuantity(CPULimit, resource.DecimalSI)
					}
				}
				serviceContainer.Resources.Limits = resourceLimit
			}
			if composeServiceConfig.Deploy.Resources.Reservations != nil {
				resourceRequests := corev1.ResourceList{}
				MemReservation := libcomposeyaml.MemStringorInt(composeServiceConfig.Deploy.Resources.Reservations.MemoryBytes)
				if MemReservation != 0 {
					resourceRequests[corev1.ResourceMemory] = *resource.NewQuantity(int64(MemReservation), "RandomStringForFormat")
				}
				if composeServiceConfig.Deploy.Resources.Reservations.NanoCPUs != "" {
					cpuReservation, err := strconv.ParseFloat(composeServiceConfig.Deploy.Resources.Reservations.NanoCPUs, 64)
					if err != nil {
						log.Warnf("Unable to convert cpu limits reservation value : %s", err)
					}
					CPUReservation := int64(cpuReservation * 1000)
					if CPUReservation != 0 {
						resourceRequests[corev1.ResourceCPU] = *resource.NewMilliQuantity(CPUReservation, resource.DecimalSI)
					}
				}
				serviceContainer.Resources.Requests = resourceRequests
			}
		}

		// HealthCheck
		if composeServiceConfig.HealthCheck != nil && !composeServiceConfig.HealthCheck.Disable {
			probe, err := c.getHealthCheck(*composeServiceConfig.HealthCheck)
			if err != nil {
				log.Warnf("Unable to parse health check : %s", err)
			} else {
				serviceContainer.LivenessProbe = &probe
			}
		}
		restart := composeServiceConfig.Restart
		if composeServiceConfig.Deploy.RestartPolicy != nil {
			restart = composeServiceConfig.Deploy.RestartPolicy.Condition
		}
		if restart == "unless-stopped" {
			log.Warnf("Restart policy 'unless-stopped' in service %s is not supported, convert it to 'always'", name)
			serviceConfig.RestartPolicy = corev1.RestartPolicyAlways
		}
		// replicas:
		if composeServiceConfig.Deploy.Replicas != nil {
			serviceConfig.Replicas = int(*composeServiceConfig.Deploy.Replicas)
		}
		serviceContainer.Env = c.getEnvs(composeServiceConfig)

		vml, vl := makeVolumesFromTmpFS(name, composeServiceConfig.Tmpfs)
		for _, v := range vl {
			serviceConfig.AddVolume(v)
		}
		serviceContainer.VolumeMounts = append(serviceContainer.VolumeMounts, vml...)

		for _, secret := range composeServiceConfig.Secrets {
			target := filepath.Join(defaultSecretBasePath, secret.Source)
			src := secret.Source
			if secret.Target != "" {
				tokens := strings.Split(secret.Source, "/")
				var prefix string
				if !strings.HasPrefix(secret.Target, "/") {
					prefix = defaultSecretBasePath + "/"
				}
				if tokens[len(tokens)-1] == secret.Target {
					target = prefix + secret.Source
				} else {
					target = prefix + strings.TrimSuffix(secret.Target, "/"+tokens[len(tokens)-1])
				}
				src = tokens[len(tokens)-1]
			}

			vSrc := corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secret.Source,
					Items: []corev1.KeyToPath{{
						Key:  secret.Source,
						Path: src,
					}},
				},
			}

			if secret.Mode != nil {
				mode := cast.ToInt32(*secret.Mode)
				vSrc.Secret.DefaultMode = &mode
			}

			serviceConfig.AddVolume(corev1.Volume{
				Name:         secret.Source,
				VolumeSource: vSrc,
			})

			serviceContainer.VolumeMounts = append(serviceContainer.VolumeMounts, corev1.VolumeMount{
				Name:      secret.Source,
				MountPath: target,
			})
		}

		for _, c := range composeServiceConfig.Configs {
			target := c.Target
			if target == "" {
				target = "/" + c.Source
			}
			vSrc := corev1.ConfigMapVolumeSource{}
			vSrc.Name = common.MakeFileNameCompliant(c.Source)
			if o, ok := composeObject.Configs[c.Source]; ok {
				if o.External.External {
					log.Errorf("Config metadata %s has an external source", c.Source)
				} else {
					srcBaseName := filepath.Base(o.File)
					vSrc.Items = []corev1.KeyToPath{{Key: srcBaseName, Path: filepath.Base(target)}}
					if c.Mode != nil {
						signedMode := int32(*c.Mode)
						vSrc.DefaultMode = &signedMode
					}
				}
			} else {
				log.Errorf("Unable to find configmap object for %s", vSrc.Name)
			}
			serviceConfig.AddVolume(corev1.Volume{
				Name:         vSrc.Name,
				VolumeSource: corev1.VolumeSource{ConfigMap: &vSrc},
			})

			serviceContainer.VolumeMounts = append(serviceContainer.VolumeMounts,
				corev1.VolumeMount{
					Name:      vSrc.Name,
					MountPath: target,
					SubPath:   filepath.Base(target),
				})
		}

		for _, vol := range composeServiceConfig.Volumes {
			if isPath(vol.Source) {
				hPath := vol.Source
				if !filepath.IsAbs(vol.Source) {
					hPath, err := filepath.Abs(vol.Source)
					if err != nil {
						log.Debugf("Could not create an absolute path for [%s]", hPath)
					}
				}
				// Generate a hash Id for the given source file path to be mounted.
				hashID := getHash([]byte(hPath))
				volumeName := fmt.Sprintf("%s%d", common.VolumePrefix, hashID)
				serviceContainer.VolumeMounts = append(serviceContainer.VolumeMounts, corev1.VolumeMount{
					Name:      volumeName,
					MountPath: vol.Target,
				})

				serviceConfig.AddVolume(corev1.Volume{
					Name: volumeName,
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: vol.Source},
					},
				})
			} else {
				serviceContainer.VolumeMounts = append(serviceContainer.VolumeMounts, corev1.VolumeMount{
					Name:      vol.Source,
					MountPath: vol.Target,
				})

				serviceConfig.AddVolume(corev1.Volume{
					Name: vol.Source,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: vol.Source,
						},
					},
				})
				storageObj := irtypes.Storage{StorageType: irtypes.PVCKind, Name: vol.Source, Content: nil}
				ir.AddStorage(storageObj)
			}
		}

		serviceConfig.Containers = []corev1.Container{serviceContainer}
		ir.Services[name] = serviceConfig
	}

	return ir, nil
}

func (c *V3Loader) getSecretStorages(secrets map[string]types.SecretConfig) []irtypes.Storage {
	storages := make([]irtypes.Storage, len(secrets))
	for secretName, secretObj := range secrets {
		storage := irtypes.Storage{
			Name:        secretName,
			StorageType: irtypes.SecretKind,
		}

		if !secretObj.External.External {
			content, err := ioutil.ReadFile(secretObj.File)
			if err != nil {
				log.Warnf("Could not read the secret file [%s]", secretObj.File)
			} else {
				storage.Content = map[string][]byte{secretName: content}
			}
		}

		storages = append(storages, storage)
	}

	return storages
}

func (c *V3Loader) getConfigStorages(configs map[string]types.ConfigObjConfig) []irtypes.Storage {
	Storages := make([]irtypes.Storage, len(configs))

	for cfgName, cfgObj := range configs {
		storage := irtypes.Storage{
			Name:        cfgName,
			StorageType: irtypes.ConfigMapKind,
		}

		if !cfgObj.External.External {
			fileInfo, err := os.Stat(cfgObj.File)
			if err != nil {
				log.Warnf("Could not identify the type of secret artifact [%s]. Encountered [%s]", cfgObj.File, err)
			} else {
				if !fileInfo.IsDir() {
					content, err := ioutil.ReadFile(cfgObj.File)
					if err != nil {
						log.Warnf("Could not read the secret file [%s]. Encountered [%s]", cfgObj.File, err)
					} else {
						storage.Content = map[string][]byte{cfgName: content}
					}
				} else {
					dataMap, err := c.getAllDirContentAsMap(cfgObj.File)
					if err != nil {
						log.Warnf("Could not read the secret directory [%s]. Encountered [%s]", cfgObj.File, err)
					} else {
						storage.Content = dataMap
					}
				}
			}
		}
		Storages = append(Storages, storage)
	}

	return Storages
}

func (*V3Loader) getPorts(ports []types.ServicePortConfig, expose []string) []corev1.ContainerPort {
	containerPorts := []corev1.ContainerPort{}
	exist := map[string]bool{}
	for _, port := range ports {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(string(corev1.ProtocolUDP), port.Protocol) {
			proto = corev1.ProtocolUDP
		}
		// Add the port to the k8s pod.
		containerPorts = append(containerPorts, corev1.ContainerPort{
			ContainerPort: int32(port.Target),
			Protocol:      proto,
		})
		exist[cast.ToString(port.Target)] = true
	}
	for _, port := range expose {
		portValue := port
		protocol := corev1.ProtocolTCP
		if strings.Contains(portValue, "/") {
			splits := strings.Split(port, "/")
			portValue = splits[0]
			protocol = corev1.Protocol(strings.ToUpper(splits[1]))
		}
		if exist[portValue] {
			continue
		}
		// Add the port to the k8s pod.
		containerPorts = append(containerPorts, corev1.ContainerPort{
			ContainerPort: cast.ToInt32(portValue),
			Protocol:      protocol,
		})
	}

	return containerPorts
}

func (*V3Loader) addPorts(ports []types.ServicePortConfig, expose []string, service *irtypes.Service) {
	exist := map[string]bool{}
	for _, port := range ports {
		// Forward the port on the k8s service to the k8s pod.
		podPort := irtypes.Port{
			Number: int32(port.Target),
		}
		servicePort := irtypes.Port{
			Number: int32(port.Published),
		}
		service.AddPortForwarding(servicePort, podPort)
		exist[cast.ToString(port.Target)] = true
	}
	for _, port := range expose {
		portValue := port
		if strings.Contains(portValue, "/") {
			splits := strings.Split(port, "/")
			portValue = splits[0]
		}
		if exist[portValue] {
			continue
		}
		// Forward the port on the k8s service to the k8s pod.
		portNumber := cast.ToInt32(portValue)
		podPort := irtypes.Port{
			Number: portNumber,
		}
		servicePort := irtypes.Port{
			Number: portNumber,
		}
		service.AddPortForwarding(servicePort, podPort)
	}
}

func (c *V3Loader) getNetworks(composeServiceConfig types.ServiceConfig, composeObject types.Config) (networks []string) {
	networks = []string{}
	for key := range composeServiceConfig.Networks {
		netName := composeObject.Networks[key].Name
		if netName == "" {
			netName = key
		}
		networks = append(networks, netName)
	}
	return networks
}

func (c *V3Loader) getHealthCheck(composeHealthCheck types.HealthCheckConfig) (corev1.Probe, error) {
	probe := corev1.Probe{}

	if len(composeHealthCheck.Test) > 1 {
		probe.Handler = corev1.Handler{
			Exec: &corev1.ExecAction{
				// docker/cli adds "CMD-SHELL" to the struct, hence we remove the first element of composeHealthCheck.Test
				Command: composeHealthCheck.Test[1:],
			},
		}
	} else {
		log.Warnf("Could not find command to execute in probe : %s", composeHealthCheck.Test)
	}
	if composeHealthCheck.Timeout != nil {
		parse, err := time.ParseDuration(composeHealthCheck.Timeout.String())
		if err != nil {
			return probe, errors.Wrap(err, "unable to parse health check timeout variable")
		}
		probe.TimeoutSeconds = int32(parse.Seconds())
	}
	if composeHealthCheck.Interval != nil {
		parse, err := time.ParseDuration(composeHealthCheck.Interval.String())
		if err != nil {
			return probe, errors.Wrap(err, "unable to parse health check interval variable")
		}
		probe.PeriodSeconds = int32(parse.Seconds())
	}
	if composeHealthCheck.Retries != nil {
		probe.FailureThreshold = int32(*composeHealthCheck.Retries)
	}
	if composeHealthCheck.StartPeriod != nil {
		parse, err := time.ParseDuration(composeHealthCheck.StartPeriod.String())
		if err != nil {
			return probe, errors.Wrap(err, "unable to parse health check startPeriod variable")
		}
		probe.InitialDelaySeconds = int32(parse.Seconds())
	}

	return probe, nil
}

func (c *V3Loader) getEnvs(composeServiceConfig types.ServiceConfig) (envs []corev1.EnvVar) {
	for name, value := range composeServiceConfig.Environment {
		var env corev1.EnvVar
		if value != nil {
			env = corev1.EnvVar{Name: name, Value: *value}
		} else {
			env = corev1.EnvVar{Name: name, Value: "unknown"}
		}
		envs = append(envs, env)
	}
	return envs
}

func (c *V3Loader) getAllDirContentAsMap(directoryPath string) (map[string][]byte, error) {
	fileList, err := ioutil.ReadDir(directoryPath)
	if err != nil {
		return nil, err
	}
	dataMap := map[string][]byte{}
	count := 0
	for _, file := range fileList {
		if file.IsDir() {
			continue
		}
		fileName := file.Name()
		log.Debugf("Reading file into the data map: [%s]", fileName)
		data, err := ioutil.ReadFile(filepath.Join(directoryPath, fileName))
		if err != nil {
			log.Debugf("Unable to read file data : %s", fileName)
			continue
		}
		dataMap[fileName] = data
		count = count + 1
	}
	log.Debugf("Read %d files into the data map", count)
	return dataMap, nil
}
