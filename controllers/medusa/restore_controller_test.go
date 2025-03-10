package medusa

import (
	"context"
	"testing"
	"time"

	cassdcapi "github.com/k8ssandra/cass-operator/apis/cassandra/v1beta1"
	k8ss "github.com/k8ssandra/k8ssandra-operator/apis/k8ssandra/v1alpha1"
	api "github.com/k8ssandra/k8ssandra-operator/apis/medusa/v1alpha1"
	"github.com/k8ssandra/k8ssandra-operator/pkg/images"
	"github.com/k8ssandra/k8ssandra-operator/test/framework"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	restoredBackupName = "backup2"
)

func testInPlaceRestore(t *testing.T, ctx context.Context, f *framework.Framework, namespace string) {
	require := require.New(t)
	err := f.Client.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(namespace))
	k8sCtx0 := "cluster-0"

	kc := &k8ss.K8ssandraCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "test",
		},
		Spec: k8ss.K8ssandraClusterSpec{
			Cassandra: &k8ss.CassandraClusterTemplate{
				Datacenters: []k8ss.CassandraDatacenterTemplate{
					{
						Meta: k8ss.EmbeddedObjectMeta{
							Name: "dc1",
						},
						K8sContext:    k8sCtx0,
						Size:          3,
						ServerVersion: "3.11.10",
						StorageConfig: &cassdcapi.StorageConfig{
							CassandraDataVolumeClaimSpec: &corev1.PersistentVolumeClaimSpec{
								StorageClassName: &defaultStorageClass,
							},
						},
					},
				},
			},
			Medusa: &api.MedusaClusterTemplate{
				ContainerImage: &images.Image{
					Repository: medusaImageRepo,
				},
				StorageProperties: api.Storage{
					StorageSecretRef: corev1.LocalObjectReference{
						Name: cassandraUserSecret,
					},
				},
				CassandraUserSecretRef: corev1.LocalObjectReference{
					Name: cassandraUserSecret,
				},
			},
		},
	}

	t.Log("Creating k8ssandracluster with Medusa")
	err = f.Client.Create(ctx, kc)
	require.NoError(err, "failed to create K8ssandraCluster")

	reconcileReplicatedSecret(ctx, t, f, kc)
	t.Log("check that dc1 was created")
	dc1Key := framework.ClusterKey{NamespacedName: types.NamespacedName{Namespace: namespace, Name: "dc1"}, K8sContext: k8sCtx0}
	require.Eventually(f.DatacenterExists(ctx, dc1Key), timeout, interval)

	t.Log("update datacenter status to scaling up")
	err = f.PatchDatacenterStatus(ctx, dc1Key, func(dc *cassdcapi.CassandraDatacenter) {
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterScalingUp,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		})
	})
	require.NoError(err, "failed to patch datacenter status")

	kcKey := framework.ClusterKey{K8sContext: k8sCtx0, NamespacedName: types.NamespacedName{Namespace: namespace, Name: "test"}}

	t.Log("check that the K8ssandraCluster status is updated")
	require.Eventually(func() bool {
		kc := &k8ss.K8ssandraCluster{}
		err = f.Get(ctx, kcKey, kc)

		if err != nil {
			t.Logf("failed to get K8ssandraCluster: %v", err)
			return false
		}

		if len(kc.Status.Datacenters) == 0 {
			return false
		}

		k8ssandraStatus, found := kc.Status.Datacenters[dc1Key.Name]
		if !found {
			t.Logf("status for datacenter %s not found", dc1Key)
			return false
		}

		condition := findDatacenterCondition(k8ssandraStatus.Cassandra, cassdcapi.DatacenterScalingUp)
		return condition != nil && condition.Status == corev1.ConditionTrue
	}, timeout, interval, "timed out waiting for K8ssandraCluster status update")

	dc1 := &cassdcapi.CassandraDatacenter{}
	err = f.Get(ctx, dc1Key, dc1)

	t.Log("update dc1 status to ready")
	err = f.PatchDatacenterStatus(ctx, dc1Key, func(dc *cassdcapi.CassandraDatacenter) {
		dc.Status.CassandraOperatorProgress = cassdcapi.ProgressReady
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		})
	})
	require.NoError(err, "failed to update dc1 status to ready")

	t.Log("creating CassandraBackup")
	backup := &api.CassandraBackup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      restoredBackupName,
		},
		Spec: api.CassandraBackupSpec{
			Name:                restoredBackupName,
			CassandraDatacenter: dc1.Name,
		},
	}

	err = f.Client.Create(ctx, backup)
	require.NoError(err, "failed to create CassandraBackup")

	restore := &api.CassandraRestore{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "test-restore",
		},
		Spec: api.CassandraRestoreSpec{
			Backup:   restoredBackupName,
			Shutdown: true,
			InPlace:  true,
			CassandraDatacenter: api.CassandraDatacenterConfig{
				Name:        "dc1",
				ClusterName: "demo",
			},
		},
	}

	restoreKey := types.NamespacedName{Namespace: restore.Namespace, Name: restore.Name}
	testClient := f.Client
	err = testClient.Create(ctx, restore)
	require.NoError(err, "failed to create CassandraRestore")

	dcKey := types.NamespacedName{Namespace: namespace, Name: "dc1"}

	withDc := newWithDatacenter(t, ctx, dcKey, testClient)

	t.Log("check that the datacenter is set to be stopped")
	require.Eventually(withDc(func(dc *cassdcapi.CassandraDatacenter) bool {
		return dc.Spec.Stopped == true
	}), timeout, interval, "timed out waiting for CassandraDatacenter stopped flag to be set")

	t.Log("delete datacenter pods to simulate shutdown")
	err = testClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(namespace), client.MatchingLabels{cassdcapi.DatacenterLabel: "dc1"})
	require.NoError(err, "failed to delete datacenter pods")

	restore = &api.CassandraRestore{}
	err = testClient.Get(ctx, restoreKey, restore)
	require.NoError(err, "failed to get CassandraRestore")

	dcStoppedTime := restore.Status.StartTime.Time.Add(1 * time.Second)

	t.Log("set datacenter status to stopped")
	err = patchDatacenterStatus(ctx, dcKey, testClient, func(dc *cassdcapi.CassandraDatacenter) {
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterStopped,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(dcStoppedTime),
		})
	})
	require.NoError(err, "failed to update datacenter status with stopped condition")

	t.Log("check that the datacenter podTemplateSpec is updated")
	require.Eventually(withDc(func(dc *cassdcapi.CassandraDatacenter) bool {
		restoreContainer := findContainer(dc.Spec.PodTemplateSpec.Spec.InitContainers, "medusa-restore")
		if restoreContainer == nil {
			t.Log("restore container not found")
			return false
		}

		envVar := findEnvVar(restoreContainer.Env, "BACKUP_NAME")
		if envVar == nil || envVar.Value != restoredBackupName {
			t.Logf("backup name not found in restore container: %v", restoreContainer.Env)
			return false
		}

		envVar = findEnvVar(restoreContainer.Env, "RESTORE_KEY")
		t.Logf("restore key: %v", envVar)
		return envVar != nil
	}), timeout, interval, "timed out waiting for CassandraDatacenter PodTemplateSpec update")

	restore = &api.CassandraRestore{}
	err = testClient.Get(ctx, restoreKey, restore)
	require.NoError(err, "failed to get CassandraRestore")

	// In addition to checking Updating condition, the restore controller also checks the
	// PodTemplateSpec of the StatefulSets to make sure the update has been pushed down.
	// Note that this test does **not** verify the StatefulSet check. cass-operator creates
	// the StatefulSets. While we could create the StatefulSets in this test, it will be
	// easier/better to verify the StatefulSet checks in unit and e2e tests.
	t.Log("set datacenter status to updated")
	err = patchDatacenterStatus(ctx, dcKey, testClient, func(dc *cassdcapi.CassandraDatacenter) {
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterUpdating,
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(restore.Status.DatacenterStopped.Add(time.Second * 1)),
		})
	})
	require.NoError(err, "failed to update datacenter status with updating condition")

	dc := &cassdcapi.CassandraDatacenter{}
	err = testClient.Get(ctx, dcKey, dc)
	require.NoError(err)

	restore = &api.CassandraRestore{}
	err = testClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "test-restore"}, restore)
	require.NoError(err)

	t.Log("check datacenter restarted")
	require.Eventually(withDc(func(dc *cassdcapi.CassandraDatacenter) bool {
		return !dc.Spec.Stopped
	}), timeout, interval)

	t.Log("set datacenter status to ready")
	err = patchDatacenterStatus(ctx, dcKey, testClient, func(dc *cassdcapi.CassandraDatacenter) {
		dc.Status.CassandraOperatorProgress = cassdcapi.ProgressReady
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(dcStoppedTime.Add(time.Second * 2)),
		})
	})

	require.NoError(err)

	t.Log("check restore status finish time set")
	require.Eventually(func() bool {
		restore := &api.CassandraRestore{}
		err := testClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "test-restore"}, restore)
		if err != nil {
			return false
		}

		return !restore.Status.FinishTime.IsZero()
	}, timeout, interval)

	err = f.DeleteK8ssandraCluster(ctx, client.ObjectKey{Namespace: kc.Namespace, Name: kc.Name})
	require.NoError(err, "failed to delete K8ssandraCluster")
}

// newWithDatacenter is a function generator for withDatacenter that is bound to t, ctx, and key.
func newWithDatacenter(t *testing.T, ctx context.Context, key types.NamespacedName, testClient client.Client) func(func(*cassdcapi.CassandraDatacenter) bool) func() bool {
	return func(condition func(dc *cassdcapi.CassandraDatacenter) bool) func() bool {
		return withDatacenter(t, ctx, key, testClient, condition)
	}
}

// withDatacenter Fetches the CassandraDatacenter specified by key and then calls condition.
func withDatacenter(t *testing.T, ctx context.Context, key types.NamespacedName, testClient client.Client, condition func(*cassdcapi.CassandraDatacenter) bool) func() bool {
	return func() bool {
		dc := &cassdcapi.CassandraDatacenter{}
		if err := testClient.Get(ctx, key, dc); err == nil {
			return condition(dc)
		} else {
			t.Logf("failed to get CassandraDatacenter: %s", err)
			return false
		}
	}
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for _, container := range containers {
		if container.Name == name {
			return &container
		}
	}
	return nil
}

func findEnvVar(envVars []corev1.EnvVar, name string) *corev1.EnvVar {
	for _, envVar := range envVars {
		if envVar.Name == name {
			return &envVar
		}
	}
	return nil
}

func patchDatacenterStatus(ctx context.Context, key types.NamespacedName, testClient client.Client, updateFn func(dc *cassdcapi.CassandraDatacenter)) error {
	dc := &cassdcapi.CassandraDatacenter{}
	err := testClient.Get(ctx, key, dc)

	if err != nil {
		return err
	}

	patch := client.MergeFromWithOptions(dc.DeepCopy(), client.MergeFromWithOptimisticLock{})
	updateFn(dc)

	return testClient.Status().Patch(ctx, dc, patch)
}
