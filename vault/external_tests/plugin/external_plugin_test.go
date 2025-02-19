package plugin_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/approle"
	"github.com/hashicorp/vault/builtin/logical/database"
	"github.com/hashicorp/vault/helper/testhelpers/consul"
	"github.com/hashicorp/vault/helper/testhelpers/corehelpers"
	postgreshelper "github.com/hashicorp/vault/helper/testhelpers/postgresql"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/sdk/helper/consts"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/vault"

	_ "github.com/jackc/pgx/v4/stdlib"
)

func getCluster(t *testing.T, typ consts.PluginType, numCores int) *vault.TestCluster {
	pluginDir, cleanup := corehelpers.MakeTestPluginDir(t)
	t.Cleanup(func() { cleanup(t) })
	coreConfig := &vault.CoreConfig{
		PluginDirectory: pluginDir,
		LogicalBackends: map[string]logical.Factory{
			"database": database.Factory,
		},
	}

	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		NumCores: numCores,
		Plugins: &vault.TestPluginConfig{
			Typ:      typ,
			Versions: []string{""},
		},
		HandlerFunc: vaulthttp.Handler,
	})

	cluster.Start()
	vault.TestWaitActive(t, cluster.Cores[0].Core)

	return cluster
}

// TestExternalPlugin_AuthMethod tests that we can build, register and use an
// external auth method
func TestExternalPlugin_AuthMethod(t *testing.T) {
	cluster := getCluster(t, consts.PluginTypeCredential, 5)
	defer cluster.Cleanup()

	plugin := cluster.Plugins[0]
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	// Register
	if err := client.Sys().RegisterPlugin(&api.RegisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Command: plugin.Name,
		SHA256:  plugin.Sha256,
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}

	// define a group of parallel tests so we wait for their execution before
	// continuing on to cleanup
	// see: https://go.dev/blog/subtests
	t.Run("parallel execution group", func(t *testing.T) {
		// loop to mount 5 auth methods that will each share a single
		// plugin process
		for i := 0; i < 5; i++ {
			i := i
			pluginPath := fmt.Sprintf("%s-%d", plugin.Name, i)
			client := cluster.Cores[i].Client
			t.Run(pluginPath, func(t *testing.T) {
				t.Parallel()
				client.SetToken(cluster.RootToken)
				// Enable
				if err := client.Sys().EnableAuthWithOptions(pluginPath, &api.EnableAuthOptions{
					Type: plugin.Name,
				}); err != nil {
					t.Fatal(err)
				}

				// Configure
				_, err := client.Logical().Write("auth/"+pluginPath+"/role/role1", map[string]interface{}{
					"bind_secret_id": "true",
					"period":         "300",
				})
				if err != nil {
					t.Fatal(err)
				}

				secret, err := client.Logical().Write("auth/"+pluginPath+"/role/role1/secret-id", nil)
				if err != nil {
					t.Fatal(err)
				}
				secretID := secret.Data["secret_id"].(string)

				secret, err = client.Logical().Read("auth/" + pluginPath + "/role/role1/role-id")
				if err != nil {
					t.Fatal(err)
				}
				roleID := secret.Data["role_id"].(string)

				// Login - expect SUCCESS
				authMethod, err := approle.NewAppRoleAuth(
					roleID,
					&approle.SecretID{FromString: secretID},
					approle.WithMountPath(pluginPath),
				)
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Auth().Login(context.Background(), authMethod)
				if err != nil {
					t.Fatal(err)
				}

				// Renew
				resp, err := client.Auth().Token().RenewSelf(30)
				if err != nil {
					t.Fatal(err)
				}

				// Login - expect SUCCESS
				resp, err = client.Auth().Login(context.Background(), authMethod)
				if err != nil {
					t.Fatal(err)
				}

				revokeToken := resp.Auth.ClientToken
				// Revoke
				if err = client.Auth().Token().RevokeSelf(revokeToken); err != nil {
					t.Fatal(err)
				}

				// Reset root token
				client.SetToken(cluster.RootToken)

				// Lookup - expect FAILURE
				resp, err = client.Auth().Token().Lookup(revokeToken)
				if err == nil {
					t.Fatalf("expected error, got nil")
				}

				// Reset root token
				client.SetToken(cluster.RootToken)
			})
		}
	})

	// Deregister
	if err := client.Sys().DeregisterPlugin(&api.DeregisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalPlugin_AuthMethodReload tests that we can use an external auth
// method after reload
func TestExternalPlugin_AuthMethodReload(t *testing.T) {
	cluster := getCluster(t, consts.PluginTypeCredential, 1)
	defer cluster.Cleanup()

	plugin := cluster.Plugins[0]
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	// Register
	if err := client.Sys().RegisterPlugin(&api.RegisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Command: plugin.Name,
		SHA256:  plugin.Sha256,
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}

	pluginPath := fmt.Sprintf("%s-%d", plugin.Name, 0)

	// Enable
	if err := client.Sys().EnableAuthWithOptions(pluginPath, &api.EnableAuthOptions{
		Type: plugin.Name,
	}); err != nil {
		t.Fatal(err)
	}

	// Configure
	_, err := client.Logical().Write("auth/"+pluginPath+"/role/role1", map[string]interface{}{
		"bind_secret_id": "true",
		"period":         "300",
	})
	if err != nil {
		t.Fatal(err)
	}

	secret, err := client.Logical().Write("auth/"+pluginPath+"/role/role1/secret-id", nil)
	if err != nil {
		t.Fatal(err)
	}
	secretID := secret.Data["secret_id"].(string)

	secret, err = client.Logical().Read("auth/" + pluginPath + "/role/role1/role-id")
	if err != nil {
		t.Fatal(err)
	}
	roleID := secret.Data["role_id"].(string)

	// Login - expect SUCCESS
	authMethod, err := approle.NewAppRoleAuth(
		roleID,
		&approle.SecretID{FromString: secretID},
		approle.WithMountPath(pluginPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Auth().Login(context.Background(), authMethod)
	if err != nil {
		t.Fatal(err)
	}

	// Reset root token
	client.SetToken(cluster.RootToken)

	// Reload plugin
	if _, err := client.Sys().ReloadPlugin(&api.ReloadPluginInput{
		Plugin: plugin.Name,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = client.Auth().Login(context.Background(), authMethod)
	if err != nil {
		t.Fatal(err)
	}

	// Reset root token
	client.SetToken(cluster.RootToken)

	// Deregister
	if err := client.Sys().DeregisterPlugin(&api.DeregisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalPlugin_SecretsEngine tests that we can build, register and use an
// external secrets engine
func TestExternalPlugin_SecretsEngine(t *testing.T) {
	cluster := getCluster(t, consts.PluginTypeSecrets, 1)
	defer cluster.Cleanup()

	plugin := cluster.Plugins[0]
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	// Register
	if err := client.Sys().RegisterPlugin(&api.RegisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Command: plugin.Name,
		SHA256:  plugin.Sha256,
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}

	// define a group of parallel tests so we wait for their execution before
	// continuing on to cleanup
	// see: https://go.dev/blog/subtests
	t.Run("parallel execution group", func(t *testing.T) {
		// loop to mount 5 secrets engines that will each share a single
		// plugin process
		for i := 0; i < 5; i++ {
			pluginPath := fmt.Sprintf("%s-%d", plugin.Name, i)
			t.Run(pluginPath, func(t *testing.T) {
				t.Parallel()
				// Enable
				if err := client.Sys().Mount(pluginPath, &api.MountInput{
					Type: plugin.Name,
				}); err != nil {
					t.Fatal(err)
				}

				// Configure
				cleanupConsul, consulConfig := consul.PrepareTestContainer(t, "", false, true)
				defer cleanupConsul()

				_, err := client.Logical().Write(pluginPath+"/config/access", map[string]interface{}{
					"address": consulConfig.Address(),
					"token":   consulConfig.Token,
				})
				if err != nil {
					t.Fatal(err)
				}

				_, err = client.Logical().Write(pluginPath+"/roles/test", map[string]interface{}{
					"consul_policies": []string{"test"},
					"ttl":             "6h",
					"local":           false,
				})
				if err != nil {
					t.Fatal(err)
				}

				resp, err := client.Logical().Read(pluginPath + "/creds/test")
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil {
					t.Fatal("read creds response is nil")
				}
			})
		}
	})

	// Deregister
	if err := client.Sys().DeregisterPlugin(&api.DeregisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalPlugin_SecretsEngineReload tests that we can use an external
// secrets engine after reload
func TestExternalPlugin_SecretsEngineReload(t *testing.T) {
	cluster := getCluster(t, consts.PluginTypeSecrets, 1)
	defer cluster.Cleanup()

	plugin := cluster.Plugins[0]
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	// Register
	if err := client.Sys().RegisterPlugin(&api.RegisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Command: plugin.Name,
		SHA256:  plugin.Sha256,
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}

	pluginPath := fmt.Sprintf("%s-%d", plugin.Name, 0)
	// Enable
	if err := client.Sys().Mount(pluginPath, &api.MountInput{
		Type: plugin.Name,
	}); err != nil {
		t.Fatal(err)
	}

	// Configure
	cleanupConsul, consulConfig := consul.PrepareTestContainer(t, "", false, true)
	defer cleanupConsul()

	_, err := client.Logical().Write(pluginPath+"/config/access", map[string]interface{}{
		"address": consulConfig.Address(),
		"token":   consulConfig.Token,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write(pluginPath+"/roles/test", map[string]interface{}{
		"consul_policies": []string{"test"},
		"ttl":             "6h",
		"local":           false,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Logical().Read(pluginPath + "/creds/test")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("read creds response is nil")
	}

	// Reload plugin
	if _, err := client.Sys().ReloadPlugin(&api.ReloadPluginInput{
		Plugin: plugin.Name,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err = client.Logical().Read(pluginPath + "/creds/test")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("read creds response is nil")
	}

	// Deregister
	if err := client.Sys().DeregisterPlugin(&api.DeregisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalPlugin_Database tests that we can build, register and use an
// external database secrets engine
func TestExternalPlugin_Database(t *testing.T) {
	cluster := getCluster(t, consts.PluginTypeDatabase, 1)
	defer cluster.Cleanup()

	plugin := cluster.Plugins[0]
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	// Register
	if err := client.Sys().RegisterPlugin(&api.RegisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(consts.PluginTypeDatabase),
		Command: plugin.Name,
		SHA256:  plugin.Sha256,
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}

	// Enable
	if err := client.Sys().Mount(consts.PluginTypeDatabase.String(), &api.MountInput{
		Type: consts.PluginTypeDatabase.String(),
	}); err != nil {
		t.Fatal(err)
	}

	// define a group of parallel tests so we wait for their execution before
	// continuing on to cleanup
	// see: https://go.dev/blog/subtests
	t.Run("parallel execution group", func(t *testing.T) {
		// loop to mount 5 database connections that will each share a single
		// plugin process
		for i := 0; i < 5; i++ {
			dbName := fmt.Sprintf("%s-%d", plugin.Name, i)
			t.Run(dbName, func(t *testing.T) {
				t.Parallel()
				roleName := "test-role-" + dbName

				cleanupContainer, connURL := postgreshelper.PrepareTestContainerWithVaultUser(t, context.Background(), "13.4-buster")
				defer cleanupContainer()

				_, err := client.Logical().Write("database/config/"+dbName, map[string]interface{}{
					"connection_url": connURL,
					"plugin_name":    plugin.Name,
					"allowed_roles":  []string{roleName},
					"username":       "vaultadmin",
					"password":       "vaultpass",
				})
				if err != nil {
					t.Fatal(err)
				}

				_, err = client.Logical().Write("database/rotate-root/"+dbName, map[string]interface{}{})
				if err != nil {
					t.Fatal(err)
				}

				_, err = client.Logical().Write("database/roles/"+roleName, map[string]interface{}{
					"db_name":             dbName,
					"creation_statements": testRole,
					"max_ttl":             "10m",
				})
				if err != nil {
					t.Fatal(err)
				}

				// Generate credentials
				resp, err := client.Logical().Read("database/creds/" + roleName)
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil {
					t.Fatal("read creds response is nil")
				}

				_, err = client.Logical().Write("database/reset/"+dbName, map[string]interface{}{})
				if err != nil {
					t.Fatal(err)
				}

				// Generate credentials
				resp, err = client.Logical().Read("database/creds/" + roleName)
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil {
					t.Fatal("read creds response is nil")
				}

				resp, err = client.Logical().Read("database/creds/" + roleName)
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil {
					t.Fatal("read creds response is nil")
				}

				revokeLease := resp.LeaseID
				// Lookup - expect SUCCESS
				resp, err = client.Sys().Lookup(revokeLease)
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil {
					t.Fatalf("lease lookup response is nil")
				}

				// Revoke
				if err = client.Sys().Revoke(revokeLease); err != nil {
					t.Fatal(err)
				}

				// Reset root token
				client.SetToken(cluster.RootToken)

				// Lookup - expect FAILURE
				resp, err = client.Sys().Lookup(revokeLease)
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
			})
		}
	})

	// Deregister
	if err := client.Sys().DeregisterPlugin(&api.DeregisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalPlugin_DatabaseReload tests that we can use an external database
// secrets engine after reload
func TestExternalPlugin_DatabaseReload(t *testing.T) {
	cluster := getCluster(t, consts.PluginTypeDatabase, 1)
	defer cluster.Cleanup()

	plugin := cluster.Plugins[0]
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	// Register
	if err := client.Sys().RegisterPlugin(&api.RegisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(consts.PluginTypeDatabase),
		Command: plugin.Name,
		SHA256:  plugin.Sha256,
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}

	// Enable
	if err := client.Sys().Mount(consts.PluginTypeDatabase.String(), &api.MountInput{
		Type: consts.PluginTypeDatabase.String(),
	}); err != nil {
		t.Fatal(err)
	}

	dbName := fmt.Sprintf("%s-%d", plugin.Name, 0)
	roleName := "test-role-" + dbName

	cleanupContainer, connURL := postgreshelper.PrepareTestContainerWithVaultUser(t, context.Background(), "13.4-buster")
	defer cleanupContainer()

	_, err := client.Logical().Write("database/config/"+dbName, map[string]interface{}{
		"connection_url": connURL,
		"plugin_name":    plugin.Name,
		"allowed_roles":  []string{roleName},
		"username":       "vaultadmin",
		"password":       "vaultpass",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("database/roles/"+roleName, map[string]interface{}{
		"db_name":             dbName,
		"creation_statements": testRole,
		"max_ttl":             "10m",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Logical().Read("database/creds/" + roleName)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("read creds response is nil")
	}

	// Reload plugin
	if _, err := client.Sys().ReloadPlugin(&api.ReloadPluginInput{
		Plugin: plugin.Name,
	}); err != nil {
		t.Fatal(err)
	}

	// Generate credentials after reload
	resp, err = client.Logical().Read("database/creds/" + roleName)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("read creds response is nil")
	}

	// Deregister
	if err := client.Sys().DeregisterPlugin(&api.DeregisterPluginInput{
		Name:    plugin.Name,
		Type:    api.PluginType(plugin.Typ),
		Version: plugin.Version,
	}); err != nil {
		t.Fatal(err)
	}
}

const testRole = `
CREATE ROLE "{{name}}" WITH
  LOGIN
  PASSWORD '{{password}}'
  VALID UNTIL '{{expiration}}';
GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO "{{name}}";
`
