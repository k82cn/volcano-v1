/*
Copyright 2017 The Volcano Authors.

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

package job

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	kbv1 "github.com/kubernetes-sigs/kube-batch/pkg/apis/scheduling/v1alpha1"
	kbver "github.com/kubernetes-sigs/kube-batch/pkg/client/clientset/versioned"
	kbinfoext "github.com/kubernetes-sigs/kube-batch/pkg/client/informers/externalversions"
	kbinfo "github.com/kubernetes-sigs/kube-batch/pkg/client/informers/externalversions/scheduling/v1alpha1"
	kblister "github.com/kubernetes-sigs/kube-batch/pkg/client/listers/scheduling/v1alpha1"

	vkapi "hpw.cloud/volcano/pkg/apis/batch/v1alpha1"
	"hpw.cloud/volcano/pkg/apis/helpers"
	"hpw.cloud/volcano/pkg/client/clientset/versioned"
	vkinfoext "hpw.cloud/volcano/pkg/client/informers/externalversions"
	vkinfo "hpw.cloud/volcano/pkg/client/informers/externalversions/batch/v1alpha1"
	vklister "hpw.cloud/volcano/pkg/client/listers/batch/v1alpha1"
)

// Controller the Job Controller type
type Controller struct {
	config      *rest.Config
	kubeClients *kubernetes.Clientset
	vkClients   *versioned.Clientset
	kbClients   *kbver.Clientset

	jobInformer vkinfo.JobInformer
	podInformer coreinformers.PodInformer
	pgInformer  kbinfo.PodGroupInformer

	// A store of jobs
	jobLister vklister.JobLister
	jobSynced func() bool

	// A store of pods, populated by the podController
	podListr  corelisters.PodLister
	podSynced func() bool

	// A store of pods, populated by the podController
	pgLister kblister.PodGroupLister
	pgSynced func() bool

	// eventQueue that need to sync up
	eventQueue *cache.FIFO
}

// NewJobController create new Job Controller
func NewJobController(config *rest.Config) *Controller {
	cc := &Controller{
		config:      config,
		kubeClients: kubernetes.NewForConfigOrDie(config),
		vkClients:   versioned.NewForConfigOrDie(config),
		kbClients:   kbver.NewForConfigOrDie(config),
		eventQueue:  cache.NewFIFO(eventKey),
	}

	cc.jobInformer = vkinfoext.NewSharedInformerFactory(cc.vkClients, 0).Batch().V1alpha1().Jobs()
	cc.jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    cc.addJob,
		UpdateFunc: cc.updateJob,
		DeleteFunc: cc.deleteJob,
	})
	cc.jobLister = cc.jobInformer.Lister()
	cc.jobSynced = cc.jobInformer.Informer().HasSynced

	cc.podInformer = informers.NewSharedInformerFactory(cc.kubeClients, 0).Core().V1().Pods()
	cc.podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    cc.addPod,
		UpdateFunc: cc.updatePod,
		DeleteFunc: cc.deletePod,
	})

	cc.podListr = cc.podInformer.Lister()
	cc.podSynced = cc.podInformer.Informer().HasSynced

	cc.pgInformer = kbinfoext.NewSharedInformerFactory(cc.kbClients, 0).Scheduling().V1alpha1().PodGroups()
	cc.pgLister = cc.pgInformer.Lister()
	cc.pgSynced = cc.pgInformer.Informer().HasSynced

	return cc
}

// Run start JobController
func (cc *Controller) Run(stopCh <-chan struct{}) {
	go cc.jobInformer.Informer().Run(stopCh)
	go cc.podInformer.Informer().Run(stopCh)
	go cc.pgInformer.Informer().Run(stopCh)

	cache.WaitForCacheSync(stopCh, cc.jobSynced, cc.podSynced, cc.pgSynced)

	go wait.Until(cc.worker, time.Second, stopCh)

	glog.Infof("JobController is running ...... ")
}

func (cc *Controller) worker() {
	if _, err := cc.eventQueue.Pop(func(obj interface{}) error {
		var job *vkapi.Job
		switch v := obj.(type) {
		case *vkapi.Job:
			job = v
		case *v1.Pod:
			jobs, err := cc.jobLister.List(labels.Everything())
			if err != nil {
				glog.Errorf("Failed to list Jobs for Pod %v/%v", v.Namespace, v.Name)
			}

			ctl := helpers.GetController(v)
			for _, j := range jobs {
				if j.UID == ctl {
					job = j
					break
				}
			}

		default:
			glog.Errorf("Un-supported type of %v", obj)
			return nil
		}

		if job == nil {
			if acc, err := meta.Accessor(obj); err != nil {
				glog.Warningf("Failed to get Job for %v/%v", acc.GetNamespace(), acc.GetName())
			}

			return nil
		}

		// sync Pods for a Job
		if err := cc.syncJob(job); err != nil {
			glog.Errorf("Failed to sync Job %s, err %#v", job.Name, err)
			// If any error, requeue it.
			return err
		}

		return nil
	}); err != nil {
		glog.Errorf("Fail to pop item from updateQueue, err %#v", err)
		return
	}
}

func (cc *Controller) syncJob(j *vkapi.Job) error {
	job, err := cc.jobLister.Jobs(j.Namespace).Get(j.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			glog.V(3).Infof("Job has been deleted: %v", j.Name)
			return nil
		}
		return err
	}

	pods, err := cc.getPodsForJob(job)
	if err != nil {
		return err
	}

	return cc.manageJob(job, pods)
}

func (cc *Controller) getPodsForJob(job *vkapi.Job) (map[string][]*v1.Pod, error) {
	pods := map[string][]*v1.Pod{}

	// TODO (k82cn): optimic by cache
	ps, err := cc.podListr.Pods(job.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, pod := range ps {
		if !metav1.IsControlledBy(pod, job) {
			continue
		}
		if len(pod.Annotations) == 0 {
			glog.Errorf("The annotations of pod <%s/%s> is empty", pod.Namespace, pod.Name)
			continue
		}
		tsName, found := pod.Annotations[vkapi.TaskSpecKey]
		if found {
			// Hash by TaskSpec.Template.Name
			pods[tsName] = append(pods[tsName], pod)
		}
	}

	return pods, nil
}

// manageJob is the core method responsible for managing the number of running
// pods according to what is specified in the job.Spec.
func (cc *Controller) manageJob(job *vkapi.Job, pods map[string][]*v1.Pod) error {
	var err error

	glog.V(3).Infof("Start to manage job <%s/%s>", job.Namespace, job.Name)

	if err := validate(job); err != nil {
		glog.Errorf("Failed to validate Job <%s/%s>: %v", job.Namespace, job.Name, err)

		/*
			job.Status = vkapi.JobStatus{
				Phase: vkapi.Error,
			}

			job.Status.Conditions = append(job.Status.Conditions, vkapi.JobCondition{})

			// TODO(k82cn): replaced it with `UpdateStatus`
			if _, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).Update(job); err != nil {
				glog.Errorf("Failed to update status of Job %v/%v: %v",
					job.Namespace, job.Name, err)
				return err
			}

			return err
		*/
	}

	runningSum := int32(0)
	pendingSum := int32(0)
	succeededSum := int32(0)
	failedSum := int32(0)

	// If PodGroup does not exist, create one for Job.
	if _, err := cc.pgLister.PodGroups((job.Namespace)).Get(job.Name); err != nil {
		if !apierrors.IsNotFound(err) {
			glog.V(3).Infof("Failed to get PodGroup for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)
			return err
		}
		pg := &kbv1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: job.Namespace,
				Name:      job.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(job, helpers.JobKind),
				},
			},
			Spec: kbv1.PodGroupSpec{
				MinMember: job.Spec.MinAvailable,
			},
		}

		if _, e := cc.kbClients.SchedulingV1alpha1().PodGroups(job.Namespace).Create(pg); e != nil {
			glog.V(3).Infof("Failed to create PodGroup for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)

			return e
		}
	}

	for _, ts := range job.Spec.TaskSpecs {
		replicas := ts.Replicas
		name := ts.Template.Name

		running := int32(filterPods(pods[name], v1.PodRunning))
		pending := int32(filterPods(pods[name], v1.PodPending))
		succeeded := int32(filterPods(pods[name], v1.PodSucceeded))
		failed := int32(filterPods(pods[name], v1.PodFailed))

		runningSum += running
		pendingSum += pending
		succeededSum += succeeded
		failedSum += failed

		glog.V(3).Infof("There are %d pods of Job %s (%s): replicas %d, pending %d, running %d, succeeded %d, failed %d",
			len(pods), job.Name, name, replicas, pending, running, succeeded, failed)

		// Create pod if necessary
		if diff := replicas - pending - running - succeeded; diff > 0 {
			glog.V(3).Infof("Try to create %v Pods for Job %v/%v", diff, job.Namespace, job.Name)

			var errs []error
			wait := sync.WaitGroup{}
			wait.Add(int(diff))
			for i := int32(0); i < diff; i++ {
				go func(ix int32) {
					defer wait.Done()
					newPod := createJobPod(job, &ts.Template, ix)
					_, err := cc.kubeClients.Core().Pods(newPod.Namespace).Create(newPod)
					if err != nil {
						// Failed to create Pod, wait a moment and then create it again
						// This is to ensure all pods under the same Job created
						// So gang-scheduling could schedule the Job successfully
						glog.Errorf("Failed to create pod %s for Job %s, err %#v",
							newPod.Name, job.Name, err)
						errs = append(errs, err)
					}
				}(i)
			}
			wait.Wait()

			if len(errs) != 0 {
				return fmt.Errorf("failed to create %d pods of %d", len(errs), diff)
			}
		}
	}

	job.Status = vkapi.JobStatus{
		Pending:      pendingSum,
		Running:      runningSum,
		Succeeded:    succeededSum,
		Failed:       failedSum,
		MinAvailable: int32(job.Spec.MinAvailable),
	}

	// TODO(k82cn): replaced it with `UpdateStatus`
	if _, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).Update(job); err != nil {
		glog.Errorf("Failed to update status of Job %v/%v: %v",
			job.Namespace, job.Name, err)
		return err
	}

	return err
}
