package main

import (
	"flag"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
	ec2Svc   ec2.EC2
	asgSvc   autoscaling.AutoScaling
}

// NewController creates a new Controller.
func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	// Load session from shared config
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		Config:            aws.Config{Region: aws.String("us-west-2")},
		SharedConfigState: session.SharedConfigEnable,
	}))

	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
		ec2Svc:   *ec2.New(sess),
		asgSvc:   *autoscaling.New(sess),
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.syncToStdout(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller.
func (c *Controller) syncToStdout(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	// Skip if event no longer exists
	if !exists {
		return nil
	}

	event := obj.(*v1.Event)

	/*
		// Debug output
		if msgBytes, err := json.Marshal(event); err == nil {
			fmt.Fprintln(os.Stdout, string(msgBytes))
		}
	*/

	// Old event, skip it
	if time.Now().Local().Unix()-event.LastTimestamp.Unix() > 120 {
		return nil
	}

	instanceId := event.ObjectMeta.Annotations["instance-id"]
	nodeName := event.InvolvedObject.Name
	klog.Infof("[%s] Spot Interruption Event received. Instance-id: %s", nodeName, instanceId)

	// Get instance info
	result, err := c.ec2Svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{&instanceId},
	})
	if err != nil {
		klog.Error("[%s] Failed to fetch instance from AWS API", nodeName, err)
		return nil
	}

	instance := result.Reservations[0].Instances[0]

	// Skip instances that aren't running
	if *instance.State.Code != 16 {
		klog.Infof("[%s] Ignoring Instance, state: %s", nodeName, *instance.State.Name)
		return nil
	}

	// Find the matching ASG
	var asg string
	tags := instance.Tags
	for _, tag := range tags {
		if *tag.Key == "aws:autoscaling:groupName" {
			asg = *tag.Value
		}
	}

	// Skip if not attached to an ASG
	if asg == "" {
		klog.Infof("[%s] Instance not attached to any ASG", nodeName)
		return nil
	}

	// Call the detach API
	klog.Infof("[%s] Detaching instance from ASG: %s", nodeName, asg)

	input := &autoscaling.DetachInstancesInput{
		AutoScalingGroupName: aws.String(asg),
		InstanceIds: []*string{
			aws.String(instanceId),
		},
		ShouldDecrementDesiredCapacity: aws.Bool(false),
	}
	detach, err := c.asgSvc.DetachInstances(input)

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeResourceContentionFault:
				fmt.Println(autoscaling.ErrCodeResourceContentionFault, aerr.Error())
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and message from an error.
			fmt.Println(err.Error())
		}
		return nil
	}

	klog.Infof("[%s] Instance Detached: %s", nodeName, detach.Activities[0].Description)

	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing event %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping event %q out of the queue: %v", key, err)
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("Starting Event controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Event controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func main() {
	var kubeconfig string
	var master string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.Parse()

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	// Create event filters
	selector := fields.ParseSelectorOrDie("involvedObject.kind=Node,reason=SpotInterruption,source=aws-node-termination-handler")

	// create the event watcher
	eventListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "events", "", selector)

	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Bind the workqueue to a cache with the help of an informer.
	indexer, informer := cache.NewIndexerInformer(eventListWatcher, &v1.Event{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer)

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	// Wait forever
	select {}
}
