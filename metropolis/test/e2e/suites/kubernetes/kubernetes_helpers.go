// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// makeTestDeploymentSpec generates a Deployment spec for a single pod running
// NGINX with a readiness probe. This allows verifying that the control plane
// is capable of scheduling simple pods and that kubelet works, its runtime is
// set up well enough to run a simple container and the network to the pod can
// pass readiness probe traffic.
func makeTestDeploymentSpec(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"name": name,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullNever,
							Image:           "bazel/metropolis/test/e2e/preseedtest:preseedtest_image",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(80)},
								},
							},
						},
					},
				},
			},
		},
	}
}

// makeHTTPServerDeploymentSpec generates the deployment spec for the test HTTP
// server.
func makeHTTPServerDeploymentSpec(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"name": name,
			}},
			Replicas: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Image:           "test.monogon.internal/httpserver:latest",
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)},
								},
							},
						},
					},
				},
			},
		},
	}
}

// makeHTTPServerNodePortService generates the NodePort service spec
// for testing the NodePort functionality.
func makeHTTPServerNodePortService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"name": name,
			},
			Ports: []corev1.ServicePort{{
				Name:       name,
				Protocol:   corev1.ProtocolTCP,
				Port:       80,
				NodePort:   80,
				TargetPort: intstr.FromInt(8080),
			}},
		},
	}
}

// makeSelftestSpec generates a Job spec for the E2E self-test image.
func makeSelftestSpec(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"job-name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Image:           "test.monogon.internal/selftest:latest",
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}
}

func sliceIf[E any](cond bool, val E) []E {
	if cond {
		return []E{val}
	}
	return nil
}

// makeTestStatefulSet generates a StatefulSet spec
func makeTestStatefulSet(name string, runtimeClass string, hostUsers *bool, hasBlock bool) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"name": name,
			}},
			VolumeClaimTemplates: slices.Concat([]corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vol-default"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceStorage: resource.MustParse("1Mi")},
						},
						VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vol-readonly"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceStorage: resource.MustParse("1Mi")},
						},
						VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem),
					},
				},
			},
				sliceIf(hasBlock, corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "vol-block"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceStorage: resource.MustParse("1Mi")},
						},
						VolumeMode: ptr.To(corev1.PersistentVolumeBlock),
					},
				}),
			),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Image:           "test.monogon.internal/persistentvolume:latest",
							Args:            slices.Concat([]string{"-name", name}, sliceIf(hasBlock, "-hasblock")),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "vol-default",
									MountPath: "/vol/default",
								},
								{
									Name:      "vol-readonly",
									ReadOnly:  true,
									MountPath: "/vol/readonly",
								},
							},
							VolumeDevices: sliceIf(hasBlock, corev1.VolumeDevice{
								Name:       "vol-block",
								DevicePath: "/vol/block",
							}),
						},
					},
					RuntimeClassName: ptr.To(runtimeClass),
					HostUsers:        hostUsers,
				},
			},
		},
	}
}

func getPodLogLines(ctx context.Context, cs kubernetes.Interface, podName string, nlines int64) ([]string, error) {
	logsR := cs.CoreV1().Pods("default").GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &nlines,
	})
	logs, err := logsR.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream failed: %w", err)
	}
	var buf bytes.Buffer
	_, err = io.Copy(&buf, logs)
	if err != nil {
		return nil, fmt.Errorf("copy failed: %w", err)
	}
	lineStr := strings.Trim(buf.String(), "\n")
	lines := strings.Split(lineStr, "\n")
	if len(lines) > int(nlines) {
		lines = lines[len(lines)-int(nlines):]
	}
	return lines, nil
}
