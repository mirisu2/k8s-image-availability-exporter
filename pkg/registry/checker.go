package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sirupsen/logrus"

	appsv1informers "k8s.io/client-go/informers/apps/v1"
	batchv1informers "k8s.io/client-go/informers/batch/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"

	"k8s.io/client-go/informers"

	"k8s.io/client-go/kubernetes"

	"github.com/flant/k8s-image-availability-exporter/pkg/store"
)

const (
	failedCheckBatchSize = 20
	checkBatchSize       = 50
)

type registryCheckerConfig struct {
	defaultRegistry string
	plainHTTP       bool
}

type Checker struct {
	imageStore *store.ImageStore

	serviceAccountInformer corev1informers.ServiceAccountInformer
	namespacesInformer     corev1informers.NamespaceInformer
	deploymentsInformer    appsv1informers.DeploymentInformer
	statefulSetsInformer   appsv1informers.StatefulSetInformer
	daemonSetsInformer     appsv1informers.DaemonSetInformer
	cronJobsInformer       batchv1informers.CronJobInformer
	secretsInformer        corev1informers.SecretInformer

	controllerIndexers ControllerIndexers

	ignoredImagesRegex []regexp.Regexp

	registryTransport *http.Transport

	kubeClient *kubernetes.Clientset

	config registryCheckerConfig
}

func NewChecker(
	stopCh <-chan struct{},
	kubeClient *kubernetes.Clientset,
	skipVerify bool,
	plainHTTP bool,
	caPths []string,
	forceCheckDisabledControllerKinds []string,
	ignoredImages []regexp.Regexp,
	defaultRegistry string,
	namespaceLabel string,
) *Checker {
	informerFactory := informers.NewSharedInformerFactory(kubeClient, time.Hour)

	customTransport := http.DefaultTransport.(*http.Transport).Clone()
	if skipVerify {
		customTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} else if len(caPths) > 0 {
		rootCAs, _ := x509.SystemCertPool()
		if rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		for _, caPath := range caPths {
			pemCerts, err := os.ReadFile(caPath)
			if err != nil {
				logrus.Fatalf("Failed to open file %q: %v", caPath, err)
			}
			if ok := rootCAs.AppendCertsFromPEM(pemCerts); !ok {
				logrus.Fatalf("Error parsing %q content as a PEM encoded certificate", caPath)
			}
		}
		customTransport.TLSClientConfig = &tls.Config{RootCAs: rootCAs}
	}

	rc := &Checker{
		serviceAccountInformer: informerFactory.Core().V1().ServiceAccounts(),
		namespacesInformer:     informerFactory.Core().V1().Namespaces(),
		deploymentsInformer:    informerFactory.Apps().V1().Deployments(),
		statefulSetsInformer:   informerFactory.Apps().V1().StatefulSets(),
		daemonSetsInformer:     informerFactory.Apps().V1().DaemonSets(),
		cronJobsInformer:       informerFactory.Batch().V1().CronJobs(),
		secretsInformer:        informerFactory.Core().V1().Secrets(),

		ignoredImagesRegex: ignoredImages,

		registryTransport: customTransport,

		kubeClient: kubeClient,

		config: registryCheckerConfig{
			defaultRegistry: defaultRegistry,
			plainHTTP:       plainHTTP,
		},
	}

	rc.imageStore = store.NewImageStore(rc.Check, checkBatchSize, failedCheckBatchSize)

	err := rc.namespacesInformer.Informer().AddIndexers(namespaceIndexers(namespaceLabel))
	if err != nil {
		panic(err)
	}
	rc.controllerIndexers.namespaceIndexer = rc.namespacesInformer.Informer().GetIndexer()

	if err != nil {
		panic(err)
	}
	rc.controllerIndexers.serviceAccountIndexer = rc.serviceAccountInformer.Informer().GetIndexer()

	_, _ = rc.deploymentsInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			rc.reconcile(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
	}, time.Minute)
	err = rc.deploymentsInformer.Informer().AddIndexers(imageIndexers)
	if err != nil {
		panic(err)
	}
	err = rc.deploymentsInformer.Informer().SetTransform(getImagesFromDeployment)
	if err != nil {
		panic(err)
	}
	rc.controllerIndexers.deploymentIndexer = rc.deploymentsInformer.Informer().GetIndexer()

	_, _ = rc.statefulSetsInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			rc.reconcile(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
	}, time.Minute)
	err = rc.statefulSetsInformer.Informer().AddIndexers(imageIndexers)
	if err != nil {
		panic(err)
	}
	err = rc.statefulSetsInformer.Informer().SetTransform(getImagesFromStatefulSet)
	if err != nil {
		panic(err)
	}
	rc.controllerIndexers.statefulSetIndexer = rc.statefulSetsInformer.Informer().GetIndexer()

	_, _ = rc.daemonSetsInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			rc.reconcile(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
	}, time.Minute)
	err = rc.daemonSetsInformer.Informer().AddIndexers(imageIndexers)
	if err != nil {
		panic(err)
	}
	err = rc.daemonSetsInformer.Informer().SetTransform(getImagesFromDaemonSet)
	if err != nil {
		panic(err)
	}
	rc.controllerIndexers.daemonSetIndexer = rc.daemonSetsInformer.Informer().GetIndexer()

	_, _ = rc.cronJobsInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			rc.reconcile(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			rc.reconcile(obj)
		},
	}, time.Minute)
	err = rc.cronJobsInformer.Informer().AddIndexers(imageIndexers)
	if err != nil {
		panic(err)
	}
	err = rc.cronJobsInformer.Informer().SetTransform(getImagesFromCronJob)
	if err != nil {
		panic(err)
	}
	rc.controllerIndexers.cronJobIndexer = rc.cronJobsInformer.Informer().GetIndexer()

	rc.controllerIndexers.secretIndexer = rc.secretsInformer.Informer().GetIndexer()

	rc.controllerIndexers.forceCheckDisabledControllerKinds = forceCheckDisabledControllerKinds

	go informerFactory.Start(stopCh)
	logrus.Info("Waiting for cache sync")
	informerFactory.WaitForCacheSync(stopCh)
	logrus.Info("Caches populated successfully")

	rc.imageStore.RunGC(rc.controllerIndexers.GetContainerInfosForImage)

	return rc
}

// Collect implements prometheus.Collector.
func (rc *Checker) Collect(ch chan<- prometheus.Metric) {
	metrics := rc.imageStore.ExtractMetrics()

	for _, m := range metrics {
		ch <- m
	}
}

// Describe implements prometheus.Collector.
func (rc *Checker) Describe(_ chan<- *prometheus.Desc) {}

func (rc *Checker) Tick() {
	rc.imageStore.Check()
}

func (rc *Checker) reconcile(obj interface{}) {
	cis := getCis(obj)

imagesLoop:
	for _, image := range cis.containerToImages {
		for _, ignoredImageRegex := range rc.ignoredImagesRegex {
			if ignoredImageRegex.MatchString(image) {
				continue imagesLoop
			}
		}

		containerInfos := rc.controllerIndexers.GetContainerInfosForImage(image)

		rc.imageStore.ReconcileImage(image, containerInfos)
	}
}

func (rc *Checker) Check(imageName string) store.AvailabilityMode {
	keyChain := rc.controllerIndexers.GetKeychainForImage(imageName)

	log := logrus.WithField("image_name", imageName)
	return rc.checkImageAvailability(log, imageName, keyChain)
}

func (rc *Checker) checkImageAvailability(log *logrus.Entry, imageName string, kc authn.Keychain) (availMode store.AvailabilityMode) {
	ref, err := parseImageName(imageName, rc.config.defaultRegistry, rc.config.plainHTTP)
	if err != nil {
		return checkImageNameParseErr(log, err)
	}

	imgErr := wait.ExponentialBackoff(wait.Backoff{
		Duration: time.Second,
		Factor:   2,
		Steps:    2,
	}, func() (bool, error) {
		var err error
		availMode, err = check(ref, kc, rc.registryTransport)

		return availMode == store.Available, err
	})

	if availMode != store.Available {
		log.WithField("availability_mode", availMode.String()).Error(imgErr)
	}

	return
}

func checkImageNameParseErr(log *logrus.Entry, err error) store.AvailabilityMode {
	var parseErr *name.ErrBadName
	if errors.As(err, &parseErr) {
		log.WithField("availability_mode", store.BadImageName.String()).Error(err)
		return store.BadImageName
	}

	log.WithField("availability_mode", store.UnknownError.String()).Error(err)
	return store.UnknownError
}

func parseImageName(image string, defaultRegistry string, plainHTTP bool) (name.Reference, error) {
	var (
		ref name.Reference
		err error
	)

	opts := make([]name.Option, 0)
	// Fallback to http scheme by default. See:
	//  go-containerregistry https://github.com/jonjohnsonjr/go-containerregistry/blob/2a0d58f7c5f77f2a03c2a0cda47fc6da26ac1564/pkg/v1/remote/transport/schemer.go#L35-L44
	if plainHTTP {
		opts = append(opts, name.Insecure)
	}

	if len(defaultRegistry) > 0 {
		opts = append(opts, name.WithDefaultRegistry(defaultRegistry))
	}

	ref, err = name.ParseReference(image, opts...)
	if err != nil {
		return nil, err
	}

	return ref, nil
}

func check(ref name.Reference, kc authn.Keychain, registryTransport *http.Transport) (store.AvailabilityMode, error) {
	var imgErr error

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Fallback to default keychain if image is not found in the provided one.
	// This is a behavior that is close to what CRI does. Because, there is maybe an image pull secret, but with
	// the wrong credentials. Yet, the image may be available with the default keychain.
	if kc != nil {
		kc = authn.NewMultiKeychain(kc, authn.DefaultKeychain)
	} else {
		kc = authn.DefaultKeychain
	}

	_, imgErr = remote.Head(
		ref,
		remote.WithAuthFromKeychain(kc),
		remote.WithTransport(registryTransport),
		remote.WithContext(ctx),
	)

	var availMode store.AvailabilityMode
	if IsAbsent(imgErr) {
		availMode = store.Absent
	} else if IsAuthnFail(imgErr) {
		availMode = store.AuthnFailure
	} else if IsAuthzFail(imgErr) {
		availMode = store.AuthzFailure
	} else if IsOldRegistry(imgErr) {
		availMode = store.Available
	} else if imgErr != nil {
		availMode = store.UnknownError
	}

	return availMode, imgErr
}
