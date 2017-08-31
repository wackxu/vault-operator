package operator

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/coreos-inc/vault-operator/pkg/spec"
	"github.com/coreos-inc/vault-operator/pkg/util/k8sutil"
	"github.com/coreos-inc/vault-operator/pkg/util/vaultutil"

	"github.com/Sirupsen/logrus"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	// Copy from deployment_controller.go:
	// maxRetries is the number of times a Vault will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a Vault is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15
)

func (v *Vaults) runWorker() {
	for v.processNextItem() {
	}
}

func (v *Vaults) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := v.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer v.queue.Done(key)

	// Invoke the method containing the business logic
	err := v.syncVault(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	v.handleErr(err, key)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (v *Vaults) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		v.queue.Forget(key)
		return
	}

	// This controller retries maxRetries times if something goes wrong. After that, it stops trying.
	if v.queue.NumRequeues(key) < maxRetries {
		logrus.Errorf("error syncing Vault (%v): %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		v.queue.AddRateLimited(key)
		return
	}

	v.queue.Forget(key)
	// Report that, even after several retries, we could not successfully process this key
	logrus.Infof("Dropping Vault (%v) out of the queue: %v", key, err)
}

// syncVault gets the vault object indexed by the key from the cache
// and initializes, reconciles or garbage collects the vault cluster as needed.
func (v *Vaults) syncVault(key string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("reconcile Vault failed: %v", err)
		}
	}()

	obj, exists, err := v.indexer.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		logrus.Infof("deleting Vault: %s", key)

		vr, exists := v.toDelete[key]
		if !exists {
			return nil
		}
		// TODO: Use a custom GC later
		err = v.deleteVault(vr)
		if err != nil {
			return err
		}

		delete(v.toDelete, key)
		return nil
	}

	// TODO: use deepcopy-gen
	cobj, err := scheme.Scheme.DeepCopy(obj)
	vr := cobj.(*spec.Vault)

	// Simulate initializer.
	// TODO: remove this when we have initializers for Vault CR.
	changed := vr.SetDefaults()
	if changed {
		vr, err = v.vaultsCRCli.Update(context.TODO(), vr)
		return err
	}

	return v.reconcileVault(vr)
}

// reconcileVault reconciles the vault cluster's state to the spec specified by vr
// by preparing the TLS secrets, deploying the etcd and vault cluster,
// and finally updating the vault deployment if needed.
func (v *Vaults) reconcileVault(vr *spec.Vault) (err error) {
	err = v.prepareDefaultVaultTLSSecrets(vr)
	if err != nil {
		return err
	}

	err = v.prepareEtcdTLSSecrets(vr)
	if err != nil {
		return err
	}

	err = v.prepareVaultConfig(vr)
	if err != nil {
		return err
	}

	err = k8sutil.DeployEtcdCluster(v.etcdCRCli, vr)
	if err != nil {
		return err
	}

	// TODO: we should do
	// if ! deployment exists -> then create deployment
	// else -> check size, version skew
	// If ! service exists -> then create service
	err = k8sutil.DeployVault(v.kubecli, vr)
	if err != nil {
		return err
	}

	err = k8sutil.UpdateVault(v.kubecli, vr)
	if err != nil {
		return err
	}

	if _, ok := v.ctxCancels[vr.Name]; !ok {
		ctx, cancel := context.WithCancel(context.Background())
		v.ctxCancels[vr.Name] = cancel
		go v.monitorAndUpdateStaus(ctx, vr)
	}

	return nil
}

// prepareVaultConfig applies our section into Vault config file.
// - If given user configmap, appends into user provided vault config
//   and creates another configmap "${configMapName}-copy" for it.
// - Otherwise, creates a new configmap "${vaultName}-copy" with our section.
func (v *Vaults) prepareVaultConfig(vr *spec.Vault) error {
	// TODO: What if user initially didn't give ConfigMapName but then update it later?

	var cfgData string
	if len(vr.Spec.ConfigMapName) != 0 {
		cm, err := v.kubecli.CoreV1().ConfigMaps(vr.Namespace).Get(vr.Spec.ConfigMapName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("prepare vault config error: get configmap (%s) failed: %v", vr.Spec.ConfigMapName, err)
		}
		cfgData = cm.Data[filepath.Base(k8sutil.VaultConfigPath)]
	}
	cfgData = vaultutil.NewConfigWithListener(cfgData)
	cfgData = vaultutil.NewConfigWithEtcd(cfgData, k8sutil.EtcdURLForVault(vr.Name))

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: k8sutil.ConfigMapNameForVault(vr),
		},
		Data: map[string]string{
			filepath.Base(k8sutil.VaultConfigPath): cfgData,
		},
	}

	_, err := v.kubecli.CoreV1().ConfigMaps(vr.Namespace).Create(cm)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("prepare vault config error: create new configmap (%s) failed: %v", cm.Name, err)
	}

	return nil
}

// TODO: replace this method with custom or k8s Garbage Collector
// deleteVault attempts to delete all associated resources with the vault cluster
func (v *Vaults) deleteVault(vr *spec.Vault) error {
	err := k8sutil.DestroyVault(v.kubecli, vr)
	if err != nil {
		return err
	}
	err = k8sutil.DeleteEtcdCluster(v.etcdCRCli, vr)
	if err != nil {
		return err
	}
	err = v.kubecli.CoreV1().ConfigMaps(vr.Namespace).Delete(
		k8sutil.ConfigMapNameForVault(vr), nil)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	err = v.cleanupEtcdTLSSecrets(vr)
	if err != nil {
		return err
	}

	err = v.cleanupDefaultVaultTLSSecrets(vr)
	return err
}