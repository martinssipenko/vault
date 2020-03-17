package command

import (
	"context"
	"encoding/base64"
	"path"
	"testing"

	wrapping "github.com/hashicorp/go-kms-wrapping"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/testhelpers"
	"github.com/hashicorp/vault/helper/testhelpers/seal"
	"github.com/hashicorp/vault/helper/testhelpers/teststorage"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/vault"
)

func verifyBarrierConfig(t *testing.T, cfg *vault.SealConfig, sealType string, shares, threshold, stored int) {
	t.Helper()
	if cfg.Type != sealType {
		t.Fatalf("bad seal config: %#v, expected type=%q", cfg, sealType)
	}
	if cfg.SecretShares != shares {
		t.Fatalf("bad seal config: %#v, expected SecretShares=%d", cfg, shares)
	}
	if cfg.SecretThreshold != threshold {
		t.Fatalf("bad seal config: %#v, expected SecretThreshold=%d", cfg, threshold)
	}
	if cfg.StoredShares != stored {
		t.Fatalf("bad seal config: %#v, expected StoredShares=%d", cfg, stored)
	}
}

func TestSealMigration_ShamirToAuto(t *testing.T) {
	t.Parallel()
	t.Run("inmem", func(t *testing.T) {
		t.Parallel()
		testSealMigrationShamirToAuto(t, teststorage.InmemBackendSetup)
	})

	t.Run("file", func(t *testing.T) {
		t.Parallel()
		testSealMigrationShamirToAuto(t, teststorage.FileBackendSetup)
	})

	t.Run("consul", func(t *testing.T) {
		t.Parallel()
		testSealMigrationShamirToAuto(t, teststorage.ConsulBackendSetup)
	})

	t.Run("raft", func(t *testing.T) {
		t.Parallel()
		testSealMigrationShamirToAuto(t, teststorage.RaftBackendSetup)
	})
}

func testSealMigrationShamirToAuto(t *testing.T, setup teststorage.ClusterSetupMutator) {
	tcluster := seal.NewTransitSealServer(t)
	defer tcluster.Cleanup()

	conf, opts := teststorage.ClusterSetup(&vault.CoreConfig{
		DisableSealWrap: true,
	}, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
		SkipInit:    true,
		NumCores:    3,
	},
		setup,
	)
	opts.SetupFunc = nil
	cluster := vault.NewTestCluster(t, conf, opts)
	tcluster.MakeKey(t, "key1")
	autoSeal := tcluster.MakeSeal(t, "key1")
	cluster.Start()
	defer cluster.Cleanup()

	client := cluster.Cores[0].Client
	initResp, err := client.Sys().Init(&api.InitRequest{
		SecretShares:    5,
		SecretThreshold: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	var resp *api.SealStatusResponse
	for _, key := range initResp.KeysB64 {
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil || !resp.Sealed {
			break
		}
	}
	if resp == nil || resp.Sealed {
		t.Fatalf("expected unsealed state; got %#v", resp)
	}

	testhelpers.WaitForActiveNode(t, cluster)
	rootToken := initResp.RootToken
	client.SetToken(rootToken)
	if err := client.Sys().Seal(); err != nil {
		t.Fatal(err)
	}

	if err := adjustCoreForSealMigration(cluster.Logger, cluster.Cores[0].Core, autoSeal, nil); err != nil {
		t.Fatal(err)
	}

	for _, key := range initResp.KeysB64 {
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key})
		if err == nil {
			t.Fatal("expected error due to lack of migrate parameter")
		}
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key, Migrate: true})
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil || !resp.Sealed {
			break
		}
	}
	if resp == nil || resp.Sealed {
		t.Fatalf("expected unsealed state; got %#v", resp)
	}

	testhelpers.WaitForActiveNode(t, cluster)
	// Seal and unseal again to verify that things are working fine
	if err := client.Sys().Seal(); err != nil {
		t.Fatal(err)
	}

	// Now the barrier unseal keys are actually the recovery keys.
	// Seal the transit cluster; we expect the unseal of our other cluster
	// to fail as a result.
	tcluster.EnsureCoresSealed(t)
	for _, key := range initResp.KeysB64 {
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key})
		if err != nil {
			break
		}
		if resp == nil || !resp.Sealed {
			break
		}
	}
	if err == nil || resp != nil {
		t.Fatalf("expected sealed state; got %#v", resp)
	}

	tcluster.UnsealCores(t)
	for _, key := range initResp.KeysB64 {
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil || !resp.Sealed {
			break
		}
	}
	if resp == nil || resp.Sealed {
		t.Fatalf("expected unsealed state; got %#v", resp)
	}

	// Make sure the seal configs were updated correctly
	b, r, err := cluster.Cores[0].Core.PhysicalSealConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifyBarrierConfig(t, b, wrapping.Transit, 1, 1, 1)
	verifyBarrierConfig(t, r, wrapping.Shamir, 5, 3, 0)
}

/*
func TestSealMigration_AutoToAuto(t *testing.T) {
	t.Parallel()
	t.Run("inmem", func(t *testing.T) {
		t.Parallel()
		testSealMigrationAutoToAuto(t, teststorage.InmemBackendSetup)
	})

	t.Run("file", func(t *testing.T) {
		t.Parallel()
		testSealMigrationAutoToAuto(t, teststorage.FileBackendSetup)
	})

	t.Run("consul", func(t *testing.T) {
		t.Parallel()
		testSealMigrationAutoToAuto(t, teststorage.ConsulBackendSetup)
	})

	t.Run("raft", func(t *testing.T) {
		t.Parallel()
		testSealMigrationAutoToAuto(t, teststorage.RaftBackendSetup)
	})
}
*/

func testSealMigrationAutoToAuto(t *testing.T, setup teststorage.ClusterSetupMutator) {
	tcluster := seal.NewTransitSealServer(t)
	defer tcluster.Cleanup()
	tcluster.MakeKey(t, "key1")
	tcluster.MakeKey(t, "key2")
	var seals []vault.Seal

	conf, opts := teststorage.ClusterSetup(&vault.CoreConfig{
		DisableSealWrap: true,
	}, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
		SkipInit:    true,
		NumCores:    3,
		SealFunc: func() vault.Seal {
			tseal := tcluster.MakeSeal(t, "key1")
			seals = append(seals, tseal)
			return tseal
		},
	},
		setup,
	)
	opts.SetupFunc = nil
	cluster := vault.NewTestCluster(t, conf, opts)
	cluster.Start()
	defer cluster.Cleanup()

	client := cluster.Cores[0].Client
	initResp, err := client.Sys().Init(&api.InitRequest{
		RecoveryShares:    5,
		RecoveryThreshold: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	rootToken := initResp.RootToken
	client.SetToken(rootToken)
	for _, k := range initResp.RecoveryKeysB64 {
		b, _ := base64.RawStdEncoding.DecodeString(k)
		cluster.RecoveryKeys = append(cluster.RecoveryKeys, b)
	}

	testhelpers.WaitForActiveNode(t, cluster)

	if err := client.Sys().Seal(); err != nil {
		t.Fatal(err)
	}

	logger := cluster.Logger.Named("shamir")
	autoSeal2 := tcluster.MakeSeal(t, "key2")
	if err := adjustCoreForSealMigration(logger, cluster.Cores[0].Core, autoSeal2, seals[0]); err != nil {
		t.Fatal(err)
	}

	// Although we're unsealing using the recovery keys, this is still an
	// autounseal; if we stopped the transit cluster this would fail.
	var resp *api.SealStatusResponse
	for _, key := range initResp.RecoveryKeysB64 {
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key})
		if err == nil {
			t.Fatal("expected error due to lack of migrate parameter")
		}
		resp, err = client.Sys().UnsealWithOptions(&api.UnsealOpts{Key: key, Migrate: true})
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil || !resp.Sealed {
			break
		}
	}
	if resp == nil || resp.Sealed {
		t.Fatalf("expected unsealed state; got %#v", resp)
	}

	testhelpers.WaitForActiveNode(t, cluster)

	// Seal and unseal again to verify that things are working fine
	if err := client.Sys().Seal(); err != nil {
		t.Fatal(err)
	}

	// Delete the original seal's transit key.
	_, err = tcluster.Cores[0].Client.Logical().Delete(path.Join("transit", "keys", "key1"))
	if err != nil {
		t.Fatal(err)
	}

	err = cluster.Cores[0].Core.UnsealWithStoredKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}
