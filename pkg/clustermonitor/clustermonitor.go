package clustermonitor

import (
	"errors"
	"fmt"
	"github.com/openshift/online/archivist/pkg/config"
	"sort"
	"time"

	oclient "github.com/openshift/origin/pkg/client"

	buildapi "github.com/openshift/origin/pkg/build/api"
	buildclient "github.com/openshift/origin/pkg/build/client/clientset_generated/internalclientset/typed/core/internalversion"

	// Prevents "no kind registered for version" even with generated clientset use
	// TODO: This shouldn't be required, may not be doing something correctly.
	_ "github.com/openshift/origin/pkg/build/api/install"

	kapi "k8s.io/kubernetes/pkg/api"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	kclientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"

	log "github.com/Sirupsen/logrus"
)

const logComponent = "clustermonitor"

func NewClusterMonitor(archivistConfig config.ArchivistConfig, clusterConfig config.ClusterConfig,
	oc oclient.Interface, kc kclientset.Interface,
	bc buildclient.CoreInterface) *ClusterMonitor {

	buildLW := &kcache.ListWatch{
		ListFunc: func(options kapi.ListOptions) (runtime.Object, error) {
			return bc.Builds(kapi.NamespaceAll).List(options)
		},
		WatchFunc: func(options kapi.ListOptions) (watch.Interface, error) {
			return bc.Builds(kapi.NamespaceAll).Watch(options)
		},
	}

	// TODO: Currently targetting 1.5 but for 1.6, deads2k suggests switching to SharedInformerFactory from
	// https://github.com/openshift/origin/blob/master/pkg/build/generated/informers/internalversion/factory.go#L29
	// Then using .Build().Builds().AddResourceEventHandler()
	buildInformer := kcache.NewSharedIndexInformer(
		buildLW,
		&buildapi.Build{},
		0, // not currently doing any re-syncing
		kcache.Indexers{
			kcache.NamespaceIndex: kcache.MetaNamespaceIndexFunc,
		},
	)

	rcLW := &kcache.ListWatch{
		ListFunc: func(options kapi.ListOptions) (runtime.Object, error) {
			return kc.Core().ReplicationControllers(kapi.NamespaceAll).List(options)
		},
		WatchFunc: func(options kapi.ListOptions) (watch.Interface, error) {
			return kc.Core().ReplicationControllers(kapi.NamespaceAll).Watch(options)
		},
	}

	rcInformer := kcache.NewSharedIndexInformer(
		rcLW,
		&kapi.ReplicationController{},
		0, // not currently doing any re-syncing
		kcache.Indexers{
			kcache.NamespaceIndex: kcache.MetaNamespaceIndexFunc,
		},
	)

	nsLW := &kcache.ListWatch{
		ListFunc: func(options kapi.ListOptions) (runtime.Object, error) {
			return kc.Core().Namespaces().List(options)
		},
		WatchFunc: func(options kapi.ListOptions) (watch.Interface, error) {
			return kc.Core().Namespaces().Watch(options)
		},
	}

	nsInformer := kcache.NewSharedIndexInformer(
		nsLW,
		&kapi.Namespace{},
		0, // not currently doing any re-syncing
		kcache.Indexers{
		//kcache.NamespaceIndex: kcache.MetaNamespaceIndexFunc,
		},
	)
	a := &ClusterMonitor{
		cfg:           archivistConfig,
		clusterCfg:    clusterConfig,
		oc:            oc,
		kc:            kc,
		bc:            bc,
		buildInformer: buildInformer,
		rcInformer:    rcInformer,
		nsInformer:    nsInformer,
		buildIndexer:  buildInformer.GetIndexer(),
		rcIndexer:     rcInformer.GetIndexer(),
		nsIndexer:     nsInformer.GetIndexer(),
	}
	return a
}

// ClusterMonitor monitors the state of the cluster and if necessary, evaluates namespace last activity to
// determine which namespaces should be archived.
type ClusterMonitor struct {
	cfg          config.ArchivistConfig
	clusterCfg   config.ClusterConfig
	oc           oclient.Interface // TODO: not used
	kc           kclientset.Interface
	bc           buildclient.CoreInterface
	stopChannel  <-chan struct{}
	buildIndexer kcache.Indexer
	rcIndexer    kcache.Indexer
	nsIndexer    kcache.Indexer

	// Avoid use in functions other than Run, the indexers are more testable:
	buildInformer kcache.SharedIndexInformer
	rcInformer    kcache.SharedIndexInformer
	nsInformer    kcache.SharedIndexInformer
}

func (a *ClusterMonitor) Run(stopChan <-chan struct{}) {
	a.stopChannel = stopChan
	go a.buildInformer.Run(a.stopChannel)
	go a.rcInformer.Run(a.stopChannel)
	go a.nsInformer.Run(a.stopChannel)

	// TODO: configurable duration
	go wait.Until(a.checkCapacity, 5*time.Minute, a.stopChannel)

	// Rather than wait a complete interval, give the informers some time to receive lists of API objects, then
	// do a capacity check:
	time.Sleep(500 * time.Millisecond)
	go a.checkCapacity()

	log.Infoln("clustermonitor is running")
}

// checkCapacity checks the capacity by all configured metrics and determines what (if any) namespaces need to
// be archived.
func (a *ClusterMonitor) checkCapacity() {
	a.getNamespacesToArchive(time.Now())
	// TODO: trigger actual archival for each namespace here
}

type LastActivity struct {
	Namespace *kapi.Namespace
	Time      time.Time
}

type LastActivitySorter []LastActivity

func (a LastActivitySorter) Len() int           { return len(a) }
func (a LastActivitySorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a LastActivitySorter) Less(i, j int) bool { return a[i].Time.Before(a[j].Time) }

func (a *ClusterMonitor) getNamespacesToArchive(checkTime time.Time) ([]LastActivity, error) {

	capLog := log.WithFields(log.Fields{
		"component": "capacitycheck",
	})
	if a.clusterCfg.NamespaceCapacity.HighWatermark == 0 {
		capLog.Warnln("no namespace capacity high watermark defined, skipping")
		return []LastActivity{}, nil
	}
	if a.clusterCfg.NamespaceCapacity.LowWatermark == 0 {
		capLog.Warnln("no namespace capacity low watermark defined, skipping")
		return []LastActivity{}, nil
	}
	// TODO: max/min inactive must be defined? or catch in config validation

	// Calculate the actual time for our activity range:
	minInactive := checkTime.AddDate(0, 0, -a.clusterCfg.MinInactiveDays)
	maxInactive := checkTime.AddDate(0, 0, -a.clusterCfg.MaxInactiveDays)

	veryInactive := make([]LastActivity, 0, 20)     // will definitely be archived
	somewhatInactive := make([]LastActivity, 0, 20) // may be archived if we need room

	// Calculate last activity time for all namespaces and sort it:

	//namespaceCount := len(namespaces)
	namespaces := a.nsIndexer.List()
	capLog.WithFields(log.Fields{
		"checkTime":     checkTime,
		"minInactive":   minInactive,
		"maxInactive":   maxInactive,
		"highWatermark": a.clusterCfg.NamespaceCapacity.HighWatermark,
		"lowWatermark":  a.clusterCfg.NamespaceCapacity.LowWatermark,
	}).Infoln("calculating namespaces to be archived")

	for _, pt := range namespaces {
		namespace := pt.(*kapi.Namespace)
		if stringInSlice(namespace.Name, a.clusterCfg.ProtectedNamespaces) {
			capLog.WithFields(log.Fields{"namespace": namespace.Name}).Debugln("skipping protected namespace")
			continue
		}
		lastActivity, err := a.getLastActivity(namespace.Name)
		if err != nil {
			return []LastActivity{}, err
		}
		if lastActivity.IsZero() {
			capLog.WithFields(log.Fields{"namespace": namespace.Name}).Warnln("no last activity time calculated for namespace")
			continue
		}
		if lastActivity.Before(maxInactive) {
			capLog.WithFields(log.Fields{
				"namespace":    namespace.Name,
				"lastActivity": lastActivity,
				"checkTime":    checkTime,
				"maxInactive":  maxInactive,
			}).Infoln("found namespace over max inactive time")
			veryInactive = append(veryInactive, LastActivity{namespace, lastActivity})
		} else if lastActivity.Before(minInactive) {
			capLog.WithFields(log.Fields{
				"namespace":    namespace.Name,
				"lastActivity": lastActivity,
				"checkTime":    checkTime,
				"minInactive":  minInactive,
				"maxInactive":  maxInactive,
			}).Infoln("found namespace between max/min inactive times")
			somewhatInactive = append(somewhatInactive, LastActivity{namespace, lastActivity})
		}
	}
	capLog.WithFields(log.Fields{
		"totalNamespaces":  len(namespaces),
		"veryInactive":     len(veryInactive),
		"somewhatInactive": len(somewhatInactive),
	}).Infoln("last activity totals")

	namespacesToArchive := make([]LastActivity, len(veryInactive), (cap(veryInactive)+1)*2)
	copy(namespacesToArchive, veryInactive)
	newNSCount := len(namespaces) - len(namespacesToArchive)

	// If the number of namespaces is over the high watermark we need to get to the low.
	// If the number of namespaces we're definitely archiving because they are very inactive
	// is not enough to get us there, we need to start archiving the somewhat inactive
	// projects:
	if len(namespaces) >= a.clusterCfg.NamespaceCapacity.HighWatermark &&
		newNSCount >= a.clusterCfg.NamespaceCapacity.LowWatermark {

		targetCount := newNSCount - a.clusterCfg.NamespaceCapacity.LowWatermark
		capLog.Debugf("looking for %d semi-inactive namespaces to archive", targetCount)
		if targetCount >= len(somewhatInactive) {
			// We don't have enough somewhat inactive namespaces to hit low watermark,
			// we can safely add all of them to the archive list:
			namespacesToArchive = append(namespacesToArchive, somewhatInactive...)
		} else {
			// Only now do we actually need to sort, and only the namespaces eligible for archival.
			// Sort into ascending order, and we will use the namespaces at the start of the slice.
			// (i.e. those with the most recent activity get to remain, despite being within the
			// threshold for archival)
			sort.Sort(LastActivitySorter(somewhatInactive))
			namespacesToArchive = append(namespacesToArchive,
				somewhatInactive[0:targetCount]...)
		}
	}
	capLog.Infof("found %d namespaces to archive", len(namespacesToArchive))
	for _, ap := range namespacesToArchive {
		capLog.Infoln("archiving:", ap.Namespace.Name)
	}
	newNSCount = len(namespaces) - len(namespacesToArchive)
	if newNSCount > a.clusterCfg.NamespaceCapacity.LowWatermark {
		capLog.WithFields(log.Fields{
			"lowWatermark": a.clusterCfg.NamespaceCapacity.LowWatermark,
			"newNSCount":   newNSCount,
		}).Warnln("unable to reach namespace capacity low watermark")
	}

	return namespacesToArchive, nil

}

// GetLastActivity returns the last activity time for a namespace by examining it's builds and replication controllers.
// If no builds or replication controllers are found we return nil. If the namespace does not exist, we return an error.
func (a *ClusterMonitor) GetLastActivity(namespace string) (time.Time, error) {
	// return an error if the namespace doesn't exist
	_, exists, err := a.nsInformer.GetIndexer().GetByKey(namespace)
	if err != nil {
		return time.Time{}, err
	}
	if !exists {
		return time.Time{}, errors.New(fmt.Sprintf("namespace does not exist in cache: %s", namespace))
	}

	tm, err := a.getLastActivity(namespace)
	return tm, err
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func (a *ClusterMonitor) getLastActivity(namespace string) (time.Time, error) {

	nsLog := log.WithFields(log.Fields{
		"namespace": namespace,
		"component": logComponent,
	})

	// Not necessarily a problem here, but worth warning about:
	if stringInSlice(namespace, a.clusterCfg.ProtectedNamespaces) {
		nsLog.Warnln("called getLastActivity for protected namespace")
	}

	var lastActivity time.Time

	builds, err := a.buildIndexer.ByIndex(kcache.NamespaceIndex, namespace)
	if err != nil {
		return time.Time{}, err
	}
	rcs, err := a.rcIndexer.ByIndex(kcache.NamespaceIndex, namespace)
	if err != nil {
		return time.Time{}, err
	}
	nsLog.WithFields(log.Fields{"builds": len(builds), "rcs": len(rcs)}).Debugln(
		"calculating last activity time")

	for _, obj := range builds {
		b := obj.(*buildapi.Build)
		// Build may briefly have no start timestamp, ignore it:
		if b.Status.StartTimestamp == nil {
			nsLog.WithFields(log.Fields{
				"namespace": namespace,
				"name":      b.Name,
				"kind":      "Build",
			}).Debugln("skipping build with no start time")
			continue
		}
		ts := b.Status.StartTimestamp
		if lastActivity.IsZero() || ts.Time.After(lastActivity) {
			lastActivity = ts.Time
			nsLog.WithFields(log.Fields{
				"lastActivity": lastActivity,
				"kind":         "Build",
				"name":         b.Name,
			}).Debugln("updating last activity time")
		}
	}

	for _, obj := range rcs {
		r := obj.(*kapi.ReplicationController)
		if r.ObjectMeta.CreationTimestamp.Time == (time.Time{}) {
			nsLog.WithFields(log.Fields{
				"name": r.Name,
				"kind": "ReplicationController",
			}).Debugln("skipping RC with no start time")
			continue
		}
		ts := &r.ObjectMeta.CreationTimestamp
		if lastActivity.IsZero() || ts.Time.After(lastActivity) {
			lastActivity = ts.Time
			nsLog.WithFields(log.Fields{
				"lastActivity": lastActivity,
				"kind":         "ReplicationController",
				"name":         r.Name,
			}).Debugln("updating namespace in cache")
		}
	}

	nsLog.WithFields(log.Fields{"lastActivity": lastActivity}).Debugln("calculated last activity")
	return lastActivity, nil
}
