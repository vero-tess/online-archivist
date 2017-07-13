package integration

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	gm "github.com/onsi/gomega"

	"github.com/openshift/online/archivist/pkg/archive"

	projectv1 "github.com/openshift/origin/pkg/project/apis/project/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/printers"
)

func getTestProjectName(prefix string) string {
	rand.Seed(time.Now().Unix())
	i := rand.Intn(10000)
	return fmt.Sprintf("%s-%d", prefix, i)
}
func testExportProjectDoesNotExist(t *testing.T, h *testHarness) {
	gm.RegisterTestingT(t)
	a := archive.NewArchiver(h.pc, h.ac, h.uc, h.uidmc, h.idc,
		h.clientFactory, h.oc, h.kc, "nosuchproject", "user")
	_, err := a.Export()
	gm.Expect(err).NotTo(gm.BeNil())
	gm.Expect(err.Error()).Should(gm.ContainSubstring("not found"))
}

func testExport(t *testing.T, h *testHarness) {
	gm.RegisterTestingT(t)
	pn := getTestProjectName("exporttest")
	log.SetLevel(log.DebugLevel)
	tlog := log.WithFields(log.Fields{
		"namespace": pn,
		"test":      "exporttest",
	})

	testProject := &projectv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pn,
			Namespace:   "",
			Annotations: map[string]string{},
		},
	}
	var err error
	testProject, err = h.pc.ProjectV1().Projects().Create(testProject)
	if err != nil {
		t.Fatal("error creating project:", err)
	}
	tlog.Info("created test project")
	defer h.pc.ProjectV1().Projects().Delete(pn, &metav1.DeleteOptions{})

	bc := buildConfig("testbc")
	bc, err = h.bc.BuildV1().BuildConfigs(pn).Create(bc)
	if err != nil {
		t.Fatal("error creating build config:", err)
	}

	dc := deploymentConfig("testdc")
	dc, err = h.deployClient.AppsV1().DeploymentConfigs(pn).Create(dc)
	if err != nil {
		t.Fatal("error creating deployment config:", err)
	}

	s := secret(pn, "testsecret")
	s, err = h.kc.Core().Secrets(pn).Create(s)
	if err != nil {
		t.Fatal("error creating secret:", err)
	}

	a := archive.NewArchiver(h.pc, h.ac, h.uc, h.uidmc, h.idc,
		h.clientFactory, h.oc, h.kc, pn, "user")
	objList, err := a.Export()
	logAll(tlog, a, objList)
	gm.Expect(err).NotTo(gm.HaveOccurred())

	expected := []string{
		"BuildConfig/testbc",
		"DeploymentConfig/testdc",
		"Secret/testsecret",
	}

	t.Run("ExpectedObjectsFound", func(t *testing.T) {
		gm.RegisterTestingT(t)
		gm.Expect(len(objList.Items)).To(gm.Equal(len(expected)))
		for _, s := range expected {
			tokens := strings.Split(s, "/")
			kind, name := tokens[0], tokens[1]
			o := findObj(t, a, objList, kind, name)
			gm.Expect(o).NotTo(gm.BeNil())
		}
	})

	t.Run("ExportedObjectsAreVersioned", func(t *testing.T) {
		gm.RegisterTestingT(t)
		// May not be the best way to test if a runtime.Object is "versioned", but this
		// is exactly how we serialize so very good coverage that the end result is what
		// we expect.
		p := printers.YAMLPrinter{}
		for _, obj := range objList.Items {
			buf := new(bytes.Buffer)
			err = p.PrintObj(obj, buf)
			if err != nil {
				gm.Expect(err).NotTo(gm.BeNil())
			}
			gm.Expect(buf.String()).To(gm.ContainSubstring("apiVersion: v1"))
		}

	})

	t.Run("SecretsExported", func(t *testing.T) {
		gm.RegisterTestingT(t)
		secretResult := findObj(t, a, objList, "Secret", s.Name)
		gm.Expect(secretResult).NotTo(gm.BeNil())
	})

	// TODO: make sure cluster info is stripped from objects
}

// findObj finds an object of the given kind and name. If not found it will return nil.
func findObj(t *testing.T, a *archive.Archiver, list *kapi.List, kind string, name string) runtime.Object {
	for _, o := range list.Items {
		if meta, err := metav1.ObjectMetaFor(o); err == nil {
			if a.ObjKind(o) == kind && meta.Name == name {
				return o
			}
		} else {
			t.Fatalf("error loading ObjectMeta for: %s", o)
			return nil
		}
	}
	return nil
}

func logAll(tlog *log.Entry, a *archive.Archiver, list *kapi.List) {
	tlog.Infoln("object list:")
	for _, o := range list.Items {
		if meta, err := metav1.ObjectMetaFor(o); err == nil {
			tlog.Infof("   %s/%s", a.ObjKind(o), meta.Name)
		} else {
			tlog.Errorf("error loading ObjectMeta for: %s", o)
		}
	}
}