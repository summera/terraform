package terraform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-test/deep"
	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/configs/hcl2shim"
	"github.com/hashicorp/terraform/plans"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/provisioners"
	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/states/statefile"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

func TestContext2Apply_basic(t *testing.T) {
	m := testModule(t, "apply-good")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) < 2 {
		t.Fatalf("bad: %#v", mod.Resources)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_unstable(t *testing.T) {
	// This tests behavior when the configuration contains an unstable value,
	// such as the result of uuid() or timestamp(), where each call produces
	// a different result.
	//
	// This is an important case to test because we need to ensure that
	// we don't re-call the function during the apply phase: the value should
	// be fixed during plan

	m := testModule(t, "apply-unstable")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("unexpected error during Plan: %s", diags.Err())
	}

	addr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_resource",
		Name: "foo",
	}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance)
	schema := p.GetSchemaReturn.ResourceTypes["test_resource"] // automatically available in mock
	rds := plan.Changes.ResourceInstance(addr)
	rd, err := rds.Decode(schema.ImpliedType())
	if err != nil {
		t.Fatal(err)
	}
	if rd.After.GetAttr("random").IsKnown() {
		t.Fatalf("Attribute 'random' has known value %#v; should be unknown in plan", rd.After.GetAttr("random"))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("unexpected error during Apply: %s", diags.Err())
	}

	mod := state.Module(addr.Module)
	rss := state.ResourceInstance(addr)

	if len(mod.Resources) != 1 {
		t.Fatalf("wrong number of resources %d; want 1", len(mod.Resources))
	}

	rs, err := rss.Current.Decode(schema.ImpliedType())
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	got := rs.Value.GetAttr("random")
	if !got.IsKnown() {
		t.Fatalf("random is still unknown after apply")
	}
	if got, want := len(got.AsString()), 36; got != want {
		t.Fatalf("random string has wrong length %d; want %d", got, want)
	}
}

func TestContext2Apply_escape(t *testing.T) {
	m := testModule(t, "apply-escape")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.bar:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = "bar"
  type = aws_instance
`)
}

func TestContext2Apply_resourceCountOneList(t *testing.T) {
	m := testModule(t, "apply-resource-count-one-list")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	assertNoDiagnostics(t, diags)

	got := strings.TrimSpace(state.String())
	want := strings.TrimSpace(`null_resource.foo.0:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/null"]

Outputs:

test = [foo]`)
	if got != want {
		t.Fatalf("got:\n%s\n\nwant:\n%s\n", got, want)
	}
}
func TestContext2Apply_resourceCountZeroList(t *testing.T) {
	m := testModule(t, "apply-resource-count-zero-list")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	got := strings.TrimSpace(state.String())
	want := strings.TrimSpace(`<no state>
Outputs:

test = []`)
	if got != want {
		t.Fatalf("wrong state\n\ngot:\n%s\n\nwant:\n%s\n", got, want)
	}
}

func TestContext2Apply_resourceDependsOnModule(t *testing.T) {
	m := testModule(t, "apply-resource-depends-on-module")
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	// verify the apply happens in the correct order
	var mu sync.Mutex
	var order []string

	p.ApplyFn = func(
		info *InstanceInfo,
		is *InstanceState,
		id *InstanceDiff) (*InstanceState, error) {

		if id.Attributes["ami"].New == "child" {

			// make the child slower than the parent
			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			order = append(order, "child")
			mu.Unlock()
		} else {
			mu.Lock()
			order = append(order, "parent")
			mu.Unlock()
		}

		return testApplyFn(info, is, id)
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if !reflect.DeepEqual(order, []string{"child", "parent"}) {
		t.Fatal("resources applied out of order")
	}

	checkStateString(t, state, testTerraformApplyResourceDependsOnModuleStr)
}

// Test that without a config, the Dependencies in the state are enough
// to maintain proper ordering.
func TestContext2Apply_resourceDependsOnModuleStateOnly(t *testing.T) {
	m := testModule(t, "apply-resource-depends-on-module-empty")
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"parent"}`),
			Dependencies: []addrs.ConfigResource{mustConfigResourceAddr("module.child.aws_instance.child")},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.child").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"child"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	{
		// verify the apply happens in the correct order
		var mu sync.Mutex
		var order []string

		p.ApplyFn = func(
			info *InstanceInfo,
			is *InstanceState,
			id *InstanceDiff) (*InstanceState, error) {

			if is.ID == "parent" {
				// make the dep slower than the parent
				time.Sleep(50 * time.Millisecond)

				mu.Lock()
				order = append(order, "child")
				mu.Unlock()
			} else {
				mu.Lock()
				order = append(order, "parent")
				mu.Unlock()
			}

			return testApplyFn(info, is, id)
		}

		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			State: state,
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags := ctx.Apply()
		assertNoErrors(t, diags)

		if !reflect.DeepEqual(order, []string{"child", "parent"}) {
			t.Fatal("resources applied out of order")
		}

		checkStateString(t, state, "<no state>")
	}
}

func TestContext2Apply_resourceDependsOnModuleDestroy(t *testing.T) {
	m := testModule(t, "apply-resource-depends-on-module")
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	var globalState *states.State
	{
		p.ApplyFn = testApplyFn
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		globalState = state
	}

	{
		// Wait for the dependency, sleep, and verify the graph never
		// called a child.
		var called int32
		var checked bool
		p.ApplyFn = func(
			info *InstanceInfo,
			is *InstanceState,
			id *InstanceDiff) (*InstanceState, error) {

			if is.Attributes["ami"] == "parent" {
				checked = true

				// Sleep to allow parallel execution
				time.Sleep(50 * time.Millisecond)

				// Verify that called is 0 (dep not called)
				if atomic.LoadInt32(&called) != 0 {
					return nil, fmt.Errorf("module child should not be called")
				}
			}

			atomic.AddInt32(&called, 1)
			return testApplyFn(info, is, id)
		}

		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			State:   globalState,
			Destroy: true,
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		if !checked {
			t.Fatal("should check")
		}

		checkStateString(t, state, `<no state>`)
	}
}

func TestContext2Apply_resourceDependsOnModuleGrandchild(t *testing.T) {
	m := testModule(t, "apply-resource-depends-on-module-deep")
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	{
		// Wait for the dependency, sleep, and verify the graph never
		// called a child.
		var called int32
		var checked bool
		p.ApplyFn = func(
			info *InstanceInfo,
			is *InstanceState,
			id *InstanceDiff) (*InstanceState, error) {

			if id.Attributes["ami"].New == "grandchild" {
				checked = true

				// Sleep to allow parallel execution
				time.Sleep(50 * time.Millisecond)

				// Verify that called is 0 (dep not called)
				if atomic.LoadInt32(&called) != 0 {
					return nil, fmt.Errorf("aws_instance.a should not be called")
				}
			}

			atomic.AddInt32(&called, 1)
			return testApplyFn(info, is, id)
		}

		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		if !checked {
			t.Fatal("should check")
		}

		checkStateString(t, state, testTerraformApplyResourceDependsOnModuleDeepStr)
	}
}

func TestContext2Apply_resourceDependsOnModuleInModule(t *testing.T) {
	m := testModule(t, "apply-resource-depends-on-module-in-module")
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	{
		// Wait for the dependency, sleep, and verify the graph never
		// called a child.
		var called int32
		var checked bool
		p.ApplyFn = func(info *InstanceInfo, is *InstanceState, id *InstanceDiff) (*InstanceState, error) {
			if id.Attributes["ami"].New == "grandchild" {
				checked = true

				// Sleep to allow parallel execution
				time.Sleep(50 * time.Millisecond)

				// Verify that called is 0 (dep not called)
				if atomic.LoadInt32(&called) != 0 {
					return nil, fmt.Errorf("something else was applied before grandchild; grandchild should be first")
				}
			}

			atomic.AddInt32(&called, 1)
			return testApplyFn(info, is, id)
		}

		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		if !checked {
			t.Fatal("should check")
		}

		checkStateString(t, state, testTerraformApplyResourceDependsOnModuleInModuleStr)
	}
}

func TestContext2Apply_mapVarBetweenModules(t *testing.T) {
	m := testModule(t, "apply-map-var-through-module")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`<no state>
Outputs:

amis_from_module = {eu-west-1:ami-789012 eu-west-2:ami-989484 us-west-1:ami-123456 us-west-2:ami-456789 }

module.test:
  null_resource.noop:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/null"]

  Outputs:

  amis_out = {eu-west-1:ami-789012 eu-west-2:ami-989484 us-west-1:ami-123456 us-west-2:ami-456789 }`)
	if actual != expected {
		t.Fatalf("expected: \n%s\n\ngot: \n%s\n", expected, actual)
	}
}

func TestContext2Apply_refCount(t *testing.T) {
	m := testModule(t, "apply-ref-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) < 2 {
		t.Fatalf("bad: %#v", mod.Resources)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyRefCountStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_providerAlias(t *testing.T) {
	m := testModule(t, "apply-provider-alias")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) < 2 {
		t.Fatalf("bad: %#v", mod.Resources)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProviderAliasStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

// Two providers that are configured should both be configured prior to apply
func TestContext2Apply_providerAliasConfigure(t *testing.T) {
	m := testModule(t, "apply-provider-alias-configure")

	p2 := testProvider("another")
	p2.ApplyFn = testApplyFn
	p2.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("another"): testProviderFuncFixed(p2),
		},
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	// Configure to record calls AFTER Plan above
	var configCount int32
	p2.ConfigureFn = func(c *ResourceConfig) error {
		atomic.AddInt32(&configCount, 1)

		foo, ok := c.Get("foo")
		if !ok {
			return fmt.Errorf("foo is not found")
		}

		if foo != "bar" {
			return fmt.Errorf("foo: %#v", foo)
		}

		return nil
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if configCount != 2 {
		t.Fatalf("provider config expected 2 calls, got: %d", configCount)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProviderAliasConfigStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

// GH-2870
func TestContext2Apply_providerWarning(t *testing.T) {
	m := testModule(t, "apply-provider-warning")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.ValidateFn = func(c *ResourceConfig) (ws []string, es []error) {
		ws = append(ws, "Just a warning")
		return
	}
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
	`)
	if actual != expected {
		t.Fatalf("got: \n%s\n\nexpected:\n%s", actual, expected)
	}

	if !p.ConfigureCalled {
		t.Fatalf("provider Configure() was never called!")
	}
}

func TestContext2Apply_emptyModule(t *testing.T) {
	// A module with only outputs (no resources)
	m := testModule(t, "apply-empty-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	actual = strings.Replace(actual, "  ", "", -1)
	expected := strings.TrimSpace(testTerraformApplyEmptyModuleStr)
	if actual != expected {
		t.Fatalf("bad: \n%s\nexpect:\n%s", actual, expected)
	}
}

func TestContext2Apply_createBeforeDestroy(t *testing.T) {
	m := testModule(t, "apply-good-create-before")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if got, want := len(mod.Resources), 1; got != want {
		t.Logf("state:\n%s", state)
		t.Fatalf("wrong number of resources %d; want %d", got, want)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyCreateBeforeStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\ngot:\n%s", expected, actual)
	}
}

func TestContext2Apply_createBeforeDestroyUpdate(t *testing.T) {
	m := testModule(t, "apply-good-create-before-update")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","foo":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) != 1 {
		t.Fatalf("bad: %s", state)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyCreateBeforeUpdateStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

// This tests that when a CBD resource depends on a non-CBD resource,
// we can still properly apply changes that require new for both.
func TestContext2Apply_createBeforeDestroy_dependsNonCBD(t *testing.T) {
	m := testModule(t, "apply-cbd-depends-non-cbd")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.bar:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  require_new = yes
  type = aws_instance
  value = foo

  Dependencies:
    aws_instance.foo
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  require_new = yes
  type = aws_instance
	`)
}

func TestContext2Apply_createBeforeDestroy_hook(t *testing.T) {
	h := new(MockHook)
	m := testModule(t, "apply-good-create-before")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	var actual []cty.Value
	var actualLock sync.Mutex
	h.PostApplyFn = func(addr addrs.AbsResourceInstance, gen states.Generation, sv cty.Value, e error) (HookAction, error) {
		actualLock.Lock()

		defer actualLock.Unlock()
		actual = append(actual, sv)
		return HookActionContinue, nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	expected := []cty.Value{
		cty.ObjectVal(map[string]cty.Value{
			"id":          cty.StringVal("foo"),
			"require_new": cty.StringVal("xyz"),
			"type":        cty.StringVal("aws_instance"),
		}),
		cty.NullVal(cty.DynamicPseudoType),
	}

	cmpOpt := cmp.Transformer("ctyshim", hcl2shim.ConfigValueFromHCL2)
	if !cmp.Equal(actual, expected, cmpOpt) {
		t.Fatalf("wrong state snapshot sequence\n%s", cmp.Diff(expected, actual, cmpOpt))
	}
}

// Test that we can perform an apply with CBD in a count with deposed instances.
func TestContext2Apply_createBeforeDestroy_deposedCount(t *testing.T) {
	m := testModule(t, "apply-cbd-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceDeposed(
		mustResourceInstanceAddr("aws_instance.bar[0]").Resource,
		states.NewDeposedKey(),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceDeposed(
		mustResourceInstanceAddr("aws_instance.bar[1]").Resource,
		states.NewDeposedKey(),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.bar.0:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = bar
  type = aws_instance
aws_instance.bar.1:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = bar
  type = aws_instance
	`)
}

// Test that when we have a deposed instance but a good primary, we still
// destroy the deposed instance.
func TestContext2Apply_createBeforeDestroy_deposedOnly(t *testing.T) {
	m := testModule(t, "apply-cbd-deposed-only")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceDeposed(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		states.NewDeposedKey(),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.bar:
  ID = bar
  provider = provider["registry.terraform.io/hashicorp/aws"]
	`)
}

func TestContext2Apply_destroyComputed(t *testing.T) {
	m := testModule(t, "apply-destroy-computed")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo", "output": "value"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	} else {
		t.Logf("plan:\n\n%s", legacyDiffComparisonString(p.Changes))
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("apply failed")
	}
}

// Test that the destroy operation uses depends_on as a source of ordering.
func TestContext2Apply_destroyDependsOn(t *testing.T) {
	// It is possible for this to be racy, so we loop a number of times
	// just to check.
	for i := 0; i < 10; i++ {
		testContext2Apply_destroyDependsOn(t)
	}
}

func testContext2Apply_destroyDependsOn(t *testing.T) {
	m := testModule(t, "apply-destroy-depends-on")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"foo"}`),
			Dependencies: []addrs.ConfigResource{mustConfigResourceAddr("aws_instance.bar")},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	// Record the order we see Apply
	var actual []string
	var actualLock sync.Mutex
	p.ApplyFn = func(
		_ *InstanceInfo, is *InstanceState, _ *InstanceDiff) (*InstanceState, error) {
		actualLock.Lock()
		defer actualLock.Unlock()
		actual = append(actual, is.ID)
		return nil, nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:       state,
		Destroy:     true,
		Parallelism: 1, // To check ordering
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	expected := []string{"foo", "bar"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("wrong order\ngot:  %#v\nwant: %#v", actual, expected)
	}
}

// Test that destroy ordering is correct with dependencies only
// in the state.
func TestContext2Apply_destroyDependsOnStateOnly(t *testing.T) {
	newState := states.NewState()
	root := newState.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "foo",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"foo"}`),
			Dependencies: []addrs.ConfigResource{},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "bar",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "foo",
					},
					Module: root.Addr.Module(),
				},
			},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)

	// It is possible for this to be racy, so we loop a number of times
	// just to check.
	for i := 0; i < 10; i++ {
		t.Run("new", func(t *testing.T) {
			testContext2Apply_destroyDependsOnStateOnly(t, newState)
		})
	}
}

func testContext2Apply_destroyDependsOnStateOnly(t *testing.T, state *states.State) {
	m := testModule(t, "empty")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	// Record the order we see Apply
	var actual []string
	var actualLock sync.Mutex
	p.ApplyFn = func(
		_ *InstanceInfo, is *InstanceState, _ *InstanceDiff) (*InstanceState, error) {
		actualLock.Lock()
		defer actualLock.Unlock()
		actual = append(actual, is.ID)
		return nil, nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:       state,
		Destroy:     true,
		Parallelism: 1, // To check ordering
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	expected := []string{"bar", "foo"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("wrong order\ngot:  %#v\nwant: %#v", actual, expected)
	}
}

// Test that destroy ordering is correct with dependencies only
// in the state within a module (GH-11749)
func TestContext2Apply_destroyDependsOnStateOnlyModule(t *testing.T) {
	newState := states.NewState()
	child := newState.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "foo",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"foo"}`),
			Dependencies: []addrs.ConfigResource{},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)
	child.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "bar",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "foo",
					},
					Module: child.Addr.Module(),
				},
			},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)

	// It is possible for this to be racy, so we loop a number of times
	// just to check.
	for i := 0; i < 10; i++ {
		t.Run("new", func(t *testing.T) {
			testContext2Apply_destroyDependsOnStateOnlyModule(t, newState)
		})
	}
}

func testContext2Apply_destroyDependsOnStateOnlyModule(t *testing.T, state *states.State) {
	m := testModule(t, "empty")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	// Record the order we see Apply
	var actual []string
	var actualLock sync.Mutex
	p.ApplyFn = func(
		_ *InstanceInfo, is *InstanceState, _ *InstanceDiff) (*InstanceState, error) {
		actualLock.Lock()
		defer actualLock.Unlock()
		actual = append(actual, is.ID)
		return nil, nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:       state,
		Destroy:     true,
		Parallelism: 1, // To check ordering
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	expected := []string{"bar", "foo"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("wrong order\ngot:  %#v\nwant: %#v", actual, expected)
	}
}

func TestContext2Apply_dataBasic(t *testing.T) {
	m := testModule(t, "apply-data-basic")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.ReadDataSourceResponse = providers.ReadDataSourceResponse{
		State: cty.ObjectVal(map[string]cty.Value{
			"id":  cty.StringVal("yo"),
			"foo": cty.NullVal(cty.String),
		}),
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	assertNoErrors(t, diags)

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyDataBasicStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_destroyData(t *testing.T) {
	m := testModule(t, "apply-destroy-data-resource")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("data.null_data_source.testing").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"-"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/null"]`),
	)

	hook := &testHook{}
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
		Hooks:   []Hook{hook},
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	newState, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if got := len(newState.Modules); got != 1 {
		t.Fatalf("state has %d modules after destroy; want 1", got)
	}

	if got := len(newState.RootModule().Resources); got != 0 {
		t.Fatalf("state has %d resources after destroy; want 0", got)
	}

	wantHookCalls := []*testHookCall{
		{"PreDiff", "data.null_data_source.testing"},
		{"PostDiff", "data.null_data_source.testing"},
		{"PostStateUpdate", ""},
	}
	if !reflect.DeepEqual(hook.Calls, wantHookCalls) {
		t.Errorf("wrong hook calls\ngot: %swant: %s", spew.Sdump(hook.Calls), spew.Sdump(wantHookCalls))
	}
}

// https://github.com/hashicorp/terraform/pull/5096
func TestContext2Apply_destroySkipsCBD(t *testing.T) {
	// Config contains CBD resource depending on non-CBD resource, which triggers
	// a cycle if they are both replaced, but should _not_ trigger a cycle when
	// just doing a `terraform destroy`.
	m := testModule(t, "apply-destroy-cbd")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_destroyModuleVarProviderConfig(t *testing.T) {
	m := testModule(t, "apply-destroy-mod-var-provider-config")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}
}

// https://github.com/hashicorp/terraform/issues/2892
func TestContext2Apply_destroyCrossProviders(t *testing.T) {
	m := testModule(t, "apply-destroy-cross-providers")

	p_aws := testProvider("aws")
	p_aws.ApplyFn = testApplyFn
	p_aws.DiffFn = testDiffFn
	p_aws.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id": {
						Type:     cty.String,
						Computed: true,
					},
				},
			},
			"aws_vpc": {
				Attributes: map[string]*configschema.Attribute{
					"id": {
						Type:     cty.String,
						Computed: true,
					},
					"value": {
						Type:     cty.String,
						Optional: true,
					},
				},
			},
		},
	}

	providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p_aws),
	}

	// Bug only appears from time to time,
	// so we run this test multiple times
	// to check for the race-condition

	// FIXME: this test flaps now, so run it more times
	for i := 0; i <= 100; i++ {
		ctx := getContextForApply_destroyCrossProviders(t, m, providers)

		if _, diags := ctx.Plan(); diags.HasErrors() {
			logDiagnostics(t, diags)
			t.Fatal("plan failed")
		}

		if _, diags := ctx.Apply(); diags.HasErrors() {
			logDiagnostics(t, diags)
			t.Fatal("apply failed")
		}
	}
}

func getContextForApply_destroyCrossProviders(t *testing.T, m *configs.Config, providerFactories map[addrs.Provider]providers.Factory) *Context {
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.shared").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"test"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_vpc.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id": "vpc-aaabbb12", "value":"test"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: providerFactories,
		State:     state,
		Destroy:   true,
	})

	return ctx
}

func TestContext2Apply_minimal(t *testing.T) {
	m := testModule(t, "apply-minimal")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyMinimalStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_badDiff(t *testing.T) {
	m := testModule(t, "apply-good")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	p.DiffFn = func(*InstanceInfo, *InstanceState, *ResourceConfig) (*InstanceDiff, error) {
		return &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{
				"newp": &ResourceAttrDiff{
					Old:         "",
					New:         "",
					NewComputed: true,
				},
			},
		}, nil
	}

	if _, diags := ctx.Apply(); diags == nil {
		t.Fatal("should error")
	}
}

func TestContext2Apply_cancel(t *testing.T) {
	stopped := false

	m := testModule(t, "apply-cancel")
	p := testProvider("aws")
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	p.ApplyFn = func(*InstanceInfo, *InstanceState, *InstanceDiff) (*InstanceState, error) {
		if !stopped {
			stopped = true
			go ctx.Stop()

			for {
				if ctx.sh.Stopped() {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}

		return &InstanceState{
			ID: "foo",
			Attributes: map[string]string{
				"value": "2",
			},
		}, nil
	}
	p.DiffFn = func(info *InstanceInfo, s *InstanceState, rc *ResourceConfig) (*InstanceDiff, error) {
		d := &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{},
		}
		if new, ok := rc.Get("value"); ok {
			d.Attributes["value"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		if new, ok := rc.Get("foo"); ok {
			d.Attributes["foo"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		return d, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	// Start the Apply in a goroutine
	var applyDiags tfdiags.Diagnostics
	stateCh := make(chan *states.State)
	go func() {
		state, diags := ctx.Apply()
		applyDiags = diags

		stateCh <- state
	}()

	state := <-stateCh
	if applyDiags.HasErrors() {
		t.Fatalf("unexpected errors: %s", applyDiags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyCancelStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	if !p.StopCalled {
		t.Fatal("stop should be called")
	}
}

func TestContext2Apply_cancelBlock(t *testing.T) {
	m := testModule(t, "apply-cancel-block")
	p := testProvider("aws")
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	applyCh := make(chan struct{})
	p.DiffFn = func(*InstanceInfo, *InstanceState, *ResourceConfig) (*InstanceDiff, error) {
		return &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{
				"id": &ResourceAttrDiff{
					New: "foo",
				},
				"num": &ResourceAttrDiff{
					New: "2",
				},
			},
		}, nil
	}
	p.ApplyFn = func(*InstanceInfo, *InstanceState, *InstanceDiff) (*InstanceState, error) {
		close(applyCh)

		for !ctx.sh.Stopped() {
			// Wait for stop to be called. We call Gosched here so that
			// the other goroutines can always be scheduled to set Stopped.
			runtime.Gosched()
		}

		// Sleep
		time.Sleep(100 * time.Millisecond)

		return &InstanceState{
			ID: "foo",
			Attributes: map[string]string{
				"num": "2",
			},
		}, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	// Start the Apply in a goroutine
	var applyDiags tfdiags.Diagnostics
	stateCh := make(chan *states.State)
	go func() {
		state, diags := ctx.Apply()
		applyDiags = diags

		stateCh <- state
	}()

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		<-applyCh
		ctx.Stop()
	}()

	// Make sure that stop blocks
	select {
	case <-stopDone:
		t.Fatal("stop should block")
	case <-time.After(10 * time.Millisecond):
	}

	// Wait for stop
	select {
	case <-stopDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stop should be done")
	}

	// Wait for apply to complete
	state := <-stateCh
	if applyDiags.HasErrors() {
		t.Fatalf("unexpected error: %s", applyDiags.Err())
	}

	checkStateString(t, state, `
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  num = 2
	`)
}

func TestContext2Apply_cancelProvisioner(t *testing.T) {
	m := testModule(t, "apply-cancel-provisioner")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr := testProvisioner()
	pr.GetSchemaResponse = provisioners.GetSchemaResponse{
		Provisioner: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"foo": {
					Type:     cty.String,
					Optional: true,
				},
			},
		},
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	prStopped := make(chan struct{})
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		// Start the stop process
		go ctx.Stop()

		<-prStopped
		return nil
	}
	pr.StopFn = func() error {
		close(prStopped)
		return nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	// Start the Apply in a goroutine
	var applyDiags tfdiags.Diagnostics
	stateCh := make(chan *states.State)
	go func() {
		state, diags := ctx.Apply()
		applyDiags = diags

		stateCh <- state
	}()

	// Wait for completion
	state := <-stateCh
	assertNoErrors(t, applyDiags)

	checkStateString(t, state, `
aws_instance.foo: (tainted)
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  num = 2
  type = aws_instance
	`)

	if !pr.StopCalled {
		t.Fatal("stop should be called")
	}
}

func TestContext2Apply_compute(t *testing.T) {
	m := testModule(t, "apply-compute")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"num": {
						Type:     cty.Number,
						Optional: true,
					},
					"compute": {
						Type:     cty.String,
						Optional: true,
					},
					"compute_value": {
						Type:     cty.String,
						Optional: true,
					},
					"foo": {
						Type:     cty.String,
						Optional: true,
					},
					"id": {
						Type:     cty.String,
						Computed: true,
					},
					"type": {
						Type:     cty.String,
						Computed: true,
					},
					"value": { // Populated from compute_value because compute = "value" in the config fixture
						Type:     cty.String,
						Computed: true,
					},
				},
			},
		},
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	ctx.variables = InputValues{
		"value": &InputValue{
			Value:      cty.NumberIntVal(1),
			SourceType: ValueFromCaller,
		},
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyComputeStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_countDecrease(t *testing.T) {
	m := testModule(t, "apply-count-dec")
	p := testProvider("aws")
	p.DiffFn = testDiffFn
	p.ApplyFn = testApplyFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","foo": "foo","type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","foo": "foo","type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[2]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "foo": "foo", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	}

	s, diags := ctx.Apply()
	assertNoErrors(t, diags)

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyCountDecStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_countDecreaseToOneX(t *testing.T) {
	m := testModule(t, "apply-count-dec-one")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "foo": "foo", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[2]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyCountDecToOneStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

// https://github.com/PeoplePerHour/terraform/pull/11
//
// This tests a case where both a "resource" and "resource.0" are in
// the state file, which apparently is a reasonable backwards compatibility
// concern found in the above 3rd party repo.
func TestContext2Apply_countDecreaseToOneCorrupted(t *testing.T) {
	m := testModule(t, "apply-count-dec-one")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "foo": "foo", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"baz", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		got := strings.TrimSpace(legacyPlanComparisonString(ctx.State(), p.Changes))
		want := strings.TrimSpace(testTerraformApplyCountDecToOneCorruptedPlanStr)
		if got != want {
			t.Fatalf("wrong plan result\ngot:\n%s\nwant:\n%s", got, want)
		}
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyCountDecToOneCorruptedStr)
	if actual != expected {
		t.Fatalf("wrong final state\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_countTainted(t *testing.T) {
	m := testModule(t, "apply-count-tainted")
	p := testProvider("aws")
	p.DiffFn = testDiffFn
	p.ApplyFn = testApplyFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"bar", "type": "aws_instance", "foo": "foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	{
		plan, diags := ctx.Plan()
		assertNoErrors(t, diags)
		got := strings.TrimSpace(legacyDiffComparisonString(plan.Changes))
		want := strings.TrimSpace(`
DESTROY/CREATE: aws_instance.foo[0]
  foo:  "foo" => "foo"
  id:   "bar" => "<computed>"
  type: "aws_instance" => "aws_instance"
CREATE: aws_instance.foo[1]
  foo:  "" => "foo"
  id:   "" => "<computed>"
  type: "" => "aws_instance"
`)
		if got != want {
			t.Fatalf("wrong plan\n\ngot:\n%s\n\nwant:\n%s", got, want)
		}
	}

	s, diags := ctx.Apply()
	assertNoErrors(t, diags)

	got := strings.TrimSpace(s.String())
	want := strings.TrimSpace(`
aws_instance.foo.0:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = foo
  type = aws_instance
aws_instance.foo.1:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = foo
  type = aws_instance
`)
	if got != want {
		t.Fatalf("wrong final state\n\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestContext2Apply_countVariable(t *testing.T) {
	m := testModule(t, "apply-count-variable")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyCountVariableStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_countVariableRef(t *testing.T) {
	m := testModule(t, "apply-count-variable-ref")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyCountVariableRefStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_provisionerInterpCount(t *testing.T) {
	// This test ensures that a provisioner can interpolate a resource count
	// even though the provisioner expression is evaluated during the plan
	// walk. https://github.com/hashicorp/terraform/issues/16840

	m, snap := testModuleWithSnapshot(t, "apply-provisioner-interp-count")

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr := testProvisioner()

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
	}

	provisioners := map[string]ProvisionerFactory{
		"local-exec": testProvisionerFuncFixed(pr),
	}
	ctx := testContext2(t, &ContextOpts{
		Config:       m,
		Providers:    Providers,
		Provisioners: provisioners,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("plan failed unexpectedly: %s", diags.Err())
	}

	// We'll marshal and unmarshal the plan here, to ensure that we have
	// a clean new context as would be created if we separately ran
	// terraform plan -out=tfplan && terraform apply tfplan
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatal(err)
	}
	ctxOpts.Providers = Providers
	ctxOpts.Provisioners = provisioners
	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("failed to create context for plan: %s", diags.Err())
	}

	// Applying the plan should now succeed
	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply failed unexpectedly: %s", diags.Err())
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner was not called")
	}
}

func TestContext2Apply_foreachVariable(t *testing.T) {
	m := testModule(t, "plan-for-each-unknown-value")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"foo": &InputValue{
				Value: cty.StringVal("hello"),
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyForEachVariableStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_moduleBasic(t *testing.T) {
	m := testModule(t, "apply-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyModuleStr)
	if actual != expected {
		t.Fatalf("bad, expected:\n%s\n\nactual:\n%s", expected, actual)
	}
}

func TestContext2Apply_moduleDestroyOrder(t *testing.T) {
	m := testModule(t, "apply-module-destroy-order")
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	// Create a custom apply function to track the order they were destroyed
	var order []string
	var orderLock sync.Mutex
	p.ApplyFn = func(
		info *InstanceInfo,
		is *InstanceState,
		id *InstanceDiff) (*InstanceState, error) {

		if is.ID == "b" {
			// Pause briefly to make any race conditions more visible, since
			// missing edges here can cause undeterministic ordering.
			time.Sleep(100 * time.Millisecond)
		}

		orderLock.Lock()
		defer orderLock.Unlock()

		order = append(order, is.ID)
		return nil, nil
	}

	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id":    {Type: cty.String, Required: true},
					"blah":  {Type: cty.String, Optional: true},
					"value": {Type: cty.String, Optional: true},
				},
			},
		},
	}

	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"a"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.b").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"b"}`),
			Dependencies: []addrs.ConfigResource{mustConfigResourceAddr("module.child.aws_instance.a")},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	expected := []string{"b", "a"}
	if !reflect.DeepEqual(order, expected) {
		t.Errorf("wrong order\ngot: %#v\nwant: %#v", order, expected)
	}

	{
		actual := strings.TrimSpace(state.String())
		expected := strings.TrimSpace(testTerraformApplyModuleDestroyOrderStr)
		if actual != expected {
			t.Errorf("wrong final state\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
		}
	}
}

func TestContext2Apply_moduleInheritAlias(t *testing.T) {
	m := testModule(t, "apply-module-provider-inherit-alias")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	p.ConfigureFn = func(c *ResourceConfig) error {
		if _, ok := c.Get("value"); !ok {
			return nil
		}

		if _, ok := c.Get("root"); ok {
			return fmt.Errorf("child should not get root")
		}

		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
<no state>
module.child:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"].eu
	`)
}

func TestContext2Apply_orphanResource(t *testing.T) {
	// This is a two-step test:
	// 1. Apply a configuration with resources that have count set.
	//    This should place the empty resource object in the state to record
	//    that each exists, and record any instances.
	// 2. Apply an empty configuration against the same state, which should
	//    then clean up both the instances and the containing resource objects.
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_thing": {},
		},
	}

	// Step 1: create the resources and instances
	m := testModule(t, "apply-orphan-resource")
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	_, diags := ctx.Plan()
	assertNoErrors(t, diags)
	state, diags := ctx.Apply()
	assertNoErrors(t, diags)

	// At this point both resources should be recorded in the state, along
	// with the single instance associated with test_thing.one.
	want := states.BuildState(func(s *states.SyncState) {
		providerAddr := addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		}
		oneAddr := addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_thing",
			Name: "one",
		}.Absolute(addrs.RootModuleInstance)
		s.SetResourceProvider(oneAddr, providerAddr)
		s.SetResourceInstanceCurrent(oneAddr.Instance(addrs.IntKey(0)), &states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{}`),
		}, providerAddr)
	})

	// compare the marshaled form to easily remove empty and nil slices
	if !statefile.StatesMarshalEqual(state, want) {
		t.Fatalf("wrong state after step 1\n%s", cmp.Diff(want, state))
	}

	// Step 2: update with an empty config, to destroy everything
	m = testModule(t, "empty")
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	_, diags = ctx.Plan()
	assertNoErrors(t, diags)
	state, diags = ctx.Apply()
	assertNoErrors(t, diags)

	// The state should now be _totally_ empty, with just an empty root module
	// (since that always exists) and no resources at all.
	want = states.NewState()
	if !cmp.Equal(state, want) {
		t.Fatalf("wrong state after step 2\ngot: %swant: %s", spew.Sdump(state), spew.Sdump(want))
	}

}

func TestContext2Apply_moduleOrphanInheritAlias(t *testing.T) {
	m := testModule(t, "apply-module-provider-inherit-alias-orphan")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	called := false
	p.ConfigureFn = func(c *ResourceConfig) error {
		called = true

		if _, ok := c.Get("child"); !ok {
			return nil
		}

		if _, ok := c.Get("root"); ok {
			return fmt.Errorf("child should not get root")
		}

		return nil
	}

	// Create a state with an orphan module
	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if !called {
		t.Fatal("must call configure")
	}

	checkStateString(t, state, "<no state>")
}

func TestContext2Apply_moduleOrphanProvider(t *testing.T) {
	m := testModule(t, "apply-module-orphan-provider-inherit")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	p.ConfigureFn = func(c *ResourceConfig) error {
		if _, ok := c.Get("value"); !ok {
			return fmt.Errorf("value is not found")
		}

		return nil
	}

	// Create a state with an orphan module
	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_moduleOrphanGrandchildProvider(t *testing.T) {
	m := testModule(t, "apply-module-orphan-provider-inherit")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	p.ConfigureFn = func(c *ResourceConfig) error {
		if _, ok := c.Get("value"); !ok {
			return fmt.Errorf("value is not found")
		}

		return nil
	}

	// Create a state with an orphan module that is nested (grandchild)
	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("parent", addrs.NoKey).Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_moduleGrandchildProvider(t *testing.T) {
	m := testModule(t, "apply-module-grandchild-provider-inherit")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var callLock sync.Mutex
	called := false
	p.ConfigureFn = func(c *ResourceConfig) error {
		if _, ok := c.Get("value"); !ok {
			return fmt.Errorf("value is not found")
		}
		callLock.Lock()
		called = true
		callLock.Unlock()

		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	callLock.Lock()
	defer callLock.Unlock()
	if called != true {
		t.Fatalf("err: configure never called")
	}
}

// This tests an issue where all the providers in a module but not
// in the root weren't being added to the root properly. In this test
// case: aws is explicitly added to root, but "test" should be added to.
// With the bug, it wasn't.
func TestContext2Apply_moduleOnlyProvider(t *testing.T) {
	m := testModule(t, "apply-module-only-provider")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pTest := testProvider("test")
	pTest.ApplyFn = testApplyFn
	pTest.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"):  testProviderFuncFixed(p),
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(pTest),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyModuleOnlyProviderStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_moduleProviderAlias(t *testing.T) {
	m := testModule(t, "apply-module-provider-alias")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyModuleProviderAliasStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_moduleProviderAliasTargets(t *testing.T) {
	m := testModule(t, "apply-module-provider-alias")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.ConfigResource{
				Module: addrs.RootModule,
				Resource: addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "nonexistent",
					Name: "thing",
				},
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`
<no state>
	`)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_moduleProviderCloseNested(t *testing.T) {
	m := testModule(t, "apply-module-provider-close-nested")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

// Tests that variables used as module vars that reference data that
// already exists in the state and requires no diff works properly. This
// fixes an issue faced where module variables were pruned because they were
// accessing "non-existent" resources (they existed, just not in the graph
// cause they weren't in the diff).
func TestContext2Apply_moduleVarRefExisting(t *testing.T) {
	m := testModule(t, "apply-ref-existing")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo","foo":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyModuleVarRefExistingStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_moduleVarResourceCount(t *testing.T) {
	m := testModule(t, "apply-module-var-resource-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"num": &InputValue{
				Value:      cty.NumberIntVal(2),
				SourceType: ValueFromCaller,
			},
		},
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"num": &InputValue{
				Value:      cty.NumberIntVal(5),
				SourceType: ValueFromCaller,
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

// GH-819
func TestContext2Apply_moduleBool(t *testing.T) {
	m := testModule(t, "apply-module-bool")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyModuleBoolStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

// Tests that a module can be targeted and everything is properly created.
// This adds to the plan test to also just verify that apply works.
func TestContext2Apply_moduleTarget(t *testing.T) {
	m := testModule(t, "plan-targeted-cross-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Child("B", addrs.NoKey),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
<no state>
module.A:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    foo = bar
    type = aws_instance

  Outputs:

  value = foo
module.B:
  aws_instance.bar:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    foo = foo
    type = aws_instance

    Dependencies:
      module.A.aws_instance.foo
	`)
}

func TestContext2Apply_multiProvider(t *testing.T) {
	m := testModule(t, "apply-multi-provider")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pDO := testProvider("do")
	pDO.ApplyFn = testApplyFn
	pDO.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			addrs.NewDefaultProvider("do"):  testProviderFuncFixed(pDO),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) < 2 {
		t.Fatalf("bad: %#v", mod.Resources)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyMultiProviderStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_multiProviderDestroy(t *testing.T) {
	m := testModule(t, "apply-multi-provider-destroy")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		Provider: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"addr": {Type: cty.String, Optional: true},
			},
		},
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"foo": {Type: cty.String, Optional: true},
				},
			},
		},
	}

	p2 := testProvider("vault")
	p2.ApplyFn = testApplyFn
	p2.DiffFn = testDiffFn
	p2.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"vault_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id": {Type: cty.String, Computed: true},
				},
			},
		},
	}

	var state *states.State

	// First, create the instances
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"):   testProviderFuncFixed(p),
				addrs.NewDefaultProvider("vault"): testProviderFuncFixed(p2),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("errors during create plan: %s", diags.Err())
		}

		s, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("errors during create apply: %s", diags.Err())
		}

		state = s
	}

	// Destroy them
	{
		// Verify that aws_instance.bar is destroyed first
		var checked bool
		var called int32
		var lock sync.Mutex
		applyFn := func(
			info *InstanceInfo,
			is *InstanceState,
			id *InstanceDiff) (*InstanceState, error) {
			lock.Lock()
			defer lock.Unlock()

			if info.Type == "aws_instance" {
				checked = true

				// Sleep to allow parallel execution
				time.Sleep(50 * time.Millisecond)

				// Verify that called is 0 (dep not called)
				if atomic.LoadInt32(&called) != 0 {
					return nil, fmt.Errorf("nothing else should be called")
				}
			}

			atomic.AddInt32(&called, 1)
			return testApplyFn(info, is, id)
		}

		// Set the apply functions
		p.ApplyFn = applyFn
		p2.ApplyFn = applyFn

		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			State:   state,
			Config:  m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"):   testProviderFuncFixed(p),
				addrs.NewDefaultProvider("vault"): testProviderFuncFixed(p2),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("errors during destroy plan: %s", diags.Err())
		}

		s, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("errors during destroy apply: %s", diags.Err())
		}

		if !checked {
			t.Fatal("should be checked")
		}

		state = s
	}

	checkStateString(t, state, `<no state>`)
}

// This is like the multiProviderDestroy test except it tests that
// dependent resources within a child module that inherit provider
// configuration are still destroyed first.
func TestContext2Apply_multiProviderDestroyChild(t *testing.T) {
	m := testModule(t, "apply-multi-provider-destroy-child")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		Provider: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"value": {Type: cty.String, Optional: true},
			},
		},
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"foo": {Type: cty.String, Optional: true},
				},
			},
		},
	}

	p2 := testProvider("vault")
	p2.ApplyFn = testApplyFn
	p2.DiffFn = testDiffFn
	p2.GetSchemaReturn = &ProviderSchema{
		Provider: &configschema.Block{},
		ResourceTypes: map[string]*configschema.Block{
			"vault_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id": {Type: cty.String, Computed: true},
				},
			},
		},
	}

	var state *states.State

	// First, create the instances
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"):   testProviderFuncFixed(p),
				addrs.NewDefaultProvider("vault"): testProviderFuncFixed(p2),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		s, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state = s
	}

	// Destroy them
	{
		// Verify that aws_instance.bar is destroyed first
		var checked bool
		var called int32
		var lock sync.Mutex
		applyFn := func(
			info *InstanceInfo,
			is *InstanceState,
			id *InstanceDiff) (*InstanceState, error) {
			lock.Lock()
			defer lock.Unlock()

			if info.Type == "aws_instance" {
				checked = true

				// Sleep to allow parallel execution
				time.Sleep(50 * time.Millisecond)

				// Verify that called is 0 (dep not called)
				if atomic.LoadInt32(&called) != 0 {
					return nil, fmt.Errorf("nothing else should be called")
				}
			}

			atomic.AddInt32(&called, 1)
			return testApplyFn(info, is, id)
		}

		// Set the apply functions
		p.ApplyFn = applyFn
		p2.ApplyFn = applyFn

		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			State:   state,
			Config:  m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"):   testProviderFuncFixed(p),
				addrs.NewDefaultProvider("vault"): testProviderFuncFixed(p2),
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		s, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		if !checked {
			t.Fatal("should be checked")
		}

		state = s
	}

	checkStateString(t, state, `
<no state>
`)
}

func TestContext2Apply_multiVar(t *testing.T) {
	m := testModule(t, "apply-multi-var")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	// First, apply with a count of 3
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"num": &InputValue{
				Value:      cty.NumberIntVal(3),
				SourceType: ValueFromCaller,
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := state.RootModule().OutputValues["output"]
	expected := cty.StringVal("bar0,bar1,bar2")
	if actual == nil || actual.Value != expected {
		t.Fatalf("wrong value\ngot:  %#v\nwant: %#v", actual.Value, expected)
	}

	t.Logf("Initial state: %s", state.String())

	// Apply again, reduce the count to 1
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			State:  state,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			Variables: InputValues{
				"num": &InputValue{
					Value:      cty.NumberIntVal(1),
					SourceType: ValueFromCaller,
				},
			},
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		t.Logf("End state: %s", state.String())

		actual := state.RootModule().OutputValues["output"]
		if actual == nil {
			t.Fatal("missing output")
		}

		expected := cty.StringVal("bar0")
		if actual.Value != expected {
			t.Fatalf("wrong value\ngot:  %#v\nwant: %#v", actual.Value, expected)
		}
	}
}

// This is a holistic test of multi-var (aka "splat variable") handling
// across several different Terraform subsystems. This is here because
// historically there were quirky differences in handling across different
// parts of Terraform and so here we want to assert the expected behavior and
// ensure that it remains consistent in future.
func TestContext2Apply_multiVarComprehensive(t *testing.T) {
	m := testModule(t, "apply-multi-var-comprehensive")
	p := testProvider("test")

	configs := map[string]*ResourceConfig{}
	var configsLock sync.Mutex

	p.ApplyFn = testApplyFn

	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		proposed := req.ProposedNewState
		configsLock.Lock()
		defer configsLock.Unlock()
		key := proposed.GetAttr("key").AsString()
		// This test was originally written using the legacy p.DiffFn interface,
		// and so the assertions below expect an old-style ResourceConfig, which
		// we'll construct via our shim for now to avoid rewriting all of the
		// assertions.
		configs[key] = NewResourceConfigShimmed(req.Config, p.GetSchemaReturn.ResourceTypes["test_thing"])

		retVals := make(map[string]cty.Value)
		for it := proposed.ElementIterator(); it.Next(); {
			idxVal, val := it.Element()
			idx := idxVal.AsString()

			switch idx {
			case "id":
				retVals[idx] = cty.UnknownVal(cty.String)
			case "name":
				retVals[idx] = cty.StringVal(key)
			default:
				retVals[idx] = val
			}
		}

		return providers.PlanResourceChangeResponse{
			PlannedState: cty.ObjectVal(retVals),
		}
	}

	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_thing": {
				Attributes: map[string]*configschema.Attribute{
					"key": {Type: cty.String, Required: true},

					"source_id":              {Type: cty.String, Optional: true},
					"source_name":            {Type: cty.String, Optional: true},
					"first_source_id":        {Type: cty.String, Optional: true},
					"first_source_name":      {Type: cty.String, Optional: true},
					"source_ids":             {Type: cty.List(cty.String), Optional: true},
					"source_names":           {Type: cty.List(cty.String), Optional: true},
					"source_ids_from_func":   {Type: cty.List(cty.String), Optional: true},
					"source_names_from_func": {Type: cty.List(cty.String), Optional: true},
					"source_ids_wrapped":     {Type: cty.List(cty.List(cty.String)), Optional: true},
					"source_names_wrapped":   {Type: cty.List(cty.List(cty.String)), Optional: true},

					"id":   {Type: cty.String, Computed: true},
					"name": {Type: cty.String, Computed: true},
				},
			},
		},
	}

	// First, apply with a count of 3
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"num": &InputValue{
				Value:      cty.NumberIntVal(3),
				SourceType: ValueFromCaller,
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatalf("errors during plan")
	}

	checkConfig := func(key string, want map[string]interface{}) {
		configsLock.Lock()
		defer configsLock.Unlock()

		if _, ok := configs[key]; !ok {
			t.Errorf("no config recorded for %s; expected a configuration", key)
			return
		}
		got := configs[key].Config
		t.Run("config for "+key, func(t *testing.T) {
			want["key"] = key // to avoid doing this for every example
			for _, problem := range deep.Equal(got, want) {
				t.Errorf(problem)
			}
		})
	}

	checkConfig("multi_count_var.0", map[string]interface{}{
		"source_id":   hcl2shim.UnknownVariableValue,
		"source_name": "source.0",
	})
	checkConfig("multi_count_var.2", map[string]interface{}{
		"source_id":   hcl2shim.UnknownVariableValue,
		"source_name": "source.2",
	})
	checkConfig("multi_count_derived.0", map[string]interface{}{
		"source_id":   hcl2shim.UnknownVariableValue,
		"source_name": "source.0",
	})
	checkConfig("multi_count_derived.2", map[string]interface{}{
		"source_id":   hcl2shim.UnknownVariableValue,
		"source_name": "source.2",
	})
	checkConfig("whole_splat", map[string]interface{}{
		"source_ids": []interface{}{
			hcl2shim.UnknownVariableValue,
			hcl2shim.UnknownVariableValue,
			hcl2shim.UnknownVariableValue,
		},
		"source_names": []interface{}{
			"source.0",
			"source.1",
			"source.2",
		},
		"source_ids_from_func": hcl2shim.UnknownVariableValue,
		"source_names_from_func": []interface{}{
			"source.0",
			"source.1",
			"source.2",
		},

		"source_ids_wrapped": []interface{}{
			[]interface{}{
				hcl2shim.UnknownVariableValue,
				hcl2shim.UnknownVariableValue,
				hcl2shim.UnknownVariableValue,
			},
		},
		"source_names_wrapped": []interface{}{
			[]interface{}{
				"source.0",
				"source.1",
				"source.2",
			},
		},

		"first_source_id":   hcl2shim.UnknownVariableValue,
		"first_source_name": "source.0",
	})
	checkConfig("child.whole_splat", map[string]interface{}{
		"source_ids": []interface{}{
			hcl2shim.UnknownVariableValue,
			hcl2shim.UnknownVariableValue,
			hcl2shim.UnknownVariableValue,
		},
		"source_names": []interface{}{
			"source.0",
			"source.1",
			"source.2",
		},

		"source_ids_wrapped": []interface{}{
			[]interface{}{
				hcl2shim.UnknownVariableValue,
				hcl2shim.UnknownVariableValue,
				hcl2shim.UnknownVariableValue,
			},
		},
		"source_names_wrapped": []interface{}{
			[]interface{}{
				"source.0",
				"source.1",
				"source.2",
			},
		},
	})

	t.Run("apply", func(t *testing.T) {
		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("error during apply: %s", diags.Err())
		}

		want := map[string]interface{}{
			"source_ids": []interface{}{"foo", "foo", "foo"},
			"source_names": []interface{}{
				"source.0",
				"source.1",
				"source.2",
			},
		}
		got := map[string]interface{}{}
		for k, s := range state.RootModule().OutputValues {
			got[k] = hcl2shim.ConfigValueFromHCL2(s.Value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf(
				"wrong outputs\ngot:  %s\nwant: %s",
				spew.Sdump(got), spew.Sdump(want),
			)
		}
	})
}

// Test that multi-var (splat) access is ordered by count, not by
// value.
func TestContext2Apply_multiVarOrder(t *testing.T) {
	m := testModule(t, "apply-multi-var-order")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	// First, apply with a count of 3
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	t.Logf("State: %s", state.String())

	actual := state.RootModule().OutputValues["should-be-11"]
	expected := cty.StringVal("index-11")
	if actual == nil || actual.Value != expected {
		t.Fatalf("wrong value\ngot:  %#v\nwant: %#v", actual.Value, expected)
	}
}

// Test that multi-var (splat) access is ordered by count, not by
// value, through interpolations.
func TestContext2Apply_multiVarOrderInterp(t *testing.T) {
	m := testModule(t, "apply-multi-var-order-interp")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	// First, apply with a count of 3
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	t.Logf("State: %s", state.String())

	actual := state.RootModule().OutputValues["should-be-11"]
	expected := cty.StringVal("baz-index-11")
	if actual == nil || actual.Value != expected {
		t.Fatalf("wrong value\ngot:  %#v\nwant: %#v", actual.Value, expected)
	}
}

// Based on GH-10440 where a graph edge wasn't properly being created
// between a modified resource and a count instance being destroyed.
func TestContext2Apply_multiVarCountDec(t *testing.T) {
	var s *states.State

	// First create resources. Nothing sneaky here.
	{
		m := testModule(t, "apply-multi-var-count-dec")
		p := testProvider("aws")
		p.ApplyFn = testApplyFn
		p.DiffFn = testDiffFn
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			Variables: InputValues{
				"num": &InputValue{
					Value:      cty.NumberIntVal(2),
					SourceType: ValueFromCaller,
				},
			},
		})

		log.Print("\n========\nStep 1 Plan\n========")
		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		log.Print("\n========\nStep 1 Apply\n========")
		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		t.Logf("Step 1 state:\n%s", state)

		s = state
	}

	// Decrease the count by 1 and verify that everything happens in the
	// right order.
	{
		m := testModule(t, "apply-multi-var-count-dec")
		p := testProvider("aws")
		p.ApplyFn = testApplyFn
		p.DiffFn = testDiffFn

		// Verify that aws_instance.bar is modified first and nothing
		// else happens at the same time.
		var checked bool
		var called int32
		var lock sync.Mutex
		p.ApplyFn = func(
			info *InstanceInfo,
			is *InstanceState,
			id *InstanceDiff) (*InstanceState, error) {
			lock.Lock()
			defer lock.Unlock()

			if id != nil && id.Attributes != nil && id.Attributes["ami"] != nil && id.Attributes["ami"].New == "special" {
				checked = true

				// Sleep to allow parallel execution
				time.Sleep(50 * time.Millisecond)

				// Verify that called is 0 (dep not called)
				if atomic.LoadInt32(&called) != 1 {
					return nil, fmt.Errorf("nothing else should be called")
				}
			}

			atomic.AddInt32(&called, 1)
			return testApplyFn(info, is, id)
		}

		ctx := testContext2(t, &ContextOpts{
			State:  s,
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			Variables: InputValues{
				"num": &InputValue{
					Value:      cty.NumberIntVal(1),
					SourceType: ValueFromCaller,
				},
			},
		})

		log.Print("\n========\nStep 2 Plan\n========")
		plan, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("plan errors: %s", diags.Err())
		}

		t.Logf("Step 2 plan:\n%s", legacyDiffComparisonString(plan.Changes))

		log.Print("\n========\nStep 2 Apply\n========")
		state, diags := ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("apply errors: %s", diags.Err())
		}

		if !checked {
			t.Error("apply never called")
		}

		t.Logf("Step 2 state:\n%s", state)

		s = state
	}
}

// Test that we can resolve a multi-var (splat) for the first resource
// created in a non-root module, which happens when the module state doesn't
// exist yet.
// https://github.com/hashicorp/terraform/issues/14438
func TestContext2Apply_multiVarMissingState(t *testing.T) {
	m := testModule(t, "apply-multi-var-missing-state")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_thing": {
				Attributes: map[string]*configschema.Attribute{
					"a_ids": {Type: cty.String, Optional: true},
					"id":    {Type: cty.String, Computed: true},
				},
			},
		},
	}

	// First, apply with a count of 3
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan failed: %s", diags.Err())
	}

	// Before the relevant bug was fixed, Tdiagsaform would panic during apply.
	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply failed: %s", diags.Err())
	}

	// If we get here with no errors or panics then our test was successful.
}

func TestContext2Apply_nilDiff(t *testing.T) {
	m := testModule(t, "apply-good")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	p.DiffFn = func(*InstanceInfo, *InstanceState, *ResourceConfig) (*InstanceDiff, error) {
		return nil, nil
	}

	if _, diags := ctx.Apply(); diags == nil {
		t.Fatal("should error")
	}
}

func TestContext2Apply_outputOrphan(t *testing.T) {
	m := testModule(t, "apply-output-orphan")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetOutputValue("foo", cty.StringVal("bar"), false)
	root.SetOutputValue("bar", cty.StringVal("baz"), false)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyOutputOrphanStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_outputOrphanModule(t *testing.T) {
	m := testModule(t, "apply-output-orphan-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyOutputOrphanModuleStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}

	// now apply with no module in the config, which should remove the
	// remaining output
	ctx = testContext2(t, &ContextOpts{
		Config: configs.NewEmptyConfig(),
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state.DeepCopy(),
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if !state.Empty() {
		t.Fatalf("wrong final state %s\nwant empty state", spew.Sdump(state))
	}
}

func TestContext2Apply_providerComputedVar(t *testing.T) {
	m := testModule(t, "apply-provider-computed")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pTest := testProvider("test")
	pTest.ApplyFn = testApplyFn
	pTest.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"):  testProviderFuncFixed(p),
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(pTest),
		},
	})

	p.ConfigureFn = func(c *ResourceConfig) error {
		if c.IsComputed("value") {
			return fmt.Errorf("value is computed")
		}

		v, ok := c.Get("value")
		if !ok {
			return fmt.Errorf("value is not found")
		}
		if v != "yes" {
			return fmt.Errorf("value is not 'yes': %v", v)
		}

		return nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_providerConfigureDisabled(t *testing.T) {
	m := testModule(t, "apply-provider-configure-disabled")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	called := false
	p.ConfigureFn = func(c *ResourceConfig) error {
		called = true

		if _, ok := c.Get("value"); !ok {
			return fmt.Errorf("value is not found")
		}

		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	if !called {
		t.Fatal("configure never called")
	}
}

func TestContext2Apply_provisionerModule(t *testing.T) {
	m := testModule(t, "apply-provisioner-module")

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr := testProvisioner()
	pr.GetSchemaResponse = provisioners.GetSchemaResponse{
		Provisioner: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"foo": {Type: cty.String, Optional: true},
			},
		},
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerModuleStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

func TestContext2Apply_Provisioner_compute(t *testing.T) {
	m := testModule(t, "apply-provisioner-compute")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok || val != "computed_value" {
			t.Fatalf("bad value for foo: %v %#v", val, c)
		}

		return nil
	}
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
		Variables: InputValues{
			"value": &InputValue{
				Value:      cty.NumberIntVal(1),
				SourceType: ValueFromCaller,
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

func TestContext2Apply_provisionerCreateFail(t *testing.T) {
	m := testModule(t, "apply-provisioner-fail-create")
	p := testProvider("aws")
	pr := testProvisioner()
	p.DiffFn = testDiffFn

	p.ApplyFn = func(info *InstanceInfo, is *InstanceState, id *InstanceDiff) (*InstanceState, error) {
		is.ID = "foo"
		return is, fmt.Errorf("error")
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should error")
	}

	got := strings.TrimSpace(state.String())
	want := strings.TrimSpace(testTerraformApplyProvisionerFailCreateStr)
	if got != want {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestContext2Apply_provisionerCreateFailNoId(t *testing.T) {
	m := testModule(t, "apply-provisioner-fail-create")
	p := testProvider("aws")
	pr := testProvisioner()
	p.DiffFn = testDiffFn

	p.ApplyFn = func(
		info *InstanceInfo,
		is *InstanceState,
		id *InstanceDiff) (*InstanceState, error) {
		return nil, fmt.Errorf("error")
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should error")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerFailCreateNoIdStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_provisionerFail(t *testing.T) {
	m := testModule(t, "apply-provisioner-fail")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr.ApplyFn = func(*InstanceState, *ResourceConfig) error {
		return fmt.Errorf("EXPLOSION")
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should error")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerFailStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_provisionerFail_createBeforeDestroy(t *testing.T) {
	m := testModule(t, "apply-provisioner-fail-create-before")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(*InstanceState, *ResourceConfig) error {
		return fmt.Errorf("EXPLOSION")
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should error")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerFailCreateBeforeDestroyStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\n:got\n%s", expected, actual)
	}
}

func TestContext2Apply_error_createBeforeDestroy(t *testing.T) {
	m := testModule(t, "apply-error-create-before")
	p := testProvider("aws")

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})
	p.ApplyFn = func(info *InstanceInfo, is *InstanceState, id *InstanceDiff) (*InstanceState, error) {
		return nil, fmt.Errorf("placeholder error from ApplyFn")
	}
	p.DiffFn = testDiffFn

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}
	if got, want := diags.Err().Error(), "placeholder error from ApplyFn"; got != want {
		// We're looking for our artificial error from ApplyFn above, whose
		// message is literally "placeholder error from ApplyFn".
		t.Fatalf("wrong error\ngot:  %s\nwant: %s", got, want)
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyErrorCreateBeforeDestroyStr)
	if actual != expected {
		t.Fatalf("wrong final state\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_errorDestroy_createBeforeDestroy(t *testing.T) {
	m := testModule(t, "apply-error-create-before")
	p := testProvider("aws")

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar", "require_new": "abc"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})
	p.ApplyFn = func(info *InstanceInfo, is *InstanceState, id *InstanceDiff) (*InstanceState, error) {
		// Fail the destroy!
		if id.Destroy {
			return is, fmt.Errorf("error")
		}

		// Create should work
		is = &InstanceState{
			ID: "foo",
			Attributes: map[string]string{
				"type":        "aws_instance",
				"require_new": "xyz",
			},
		}
		return is, nil
	}
	p.DiffFn = testDiffFn

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyErrorDestroyCreateBeforeDestroyStr)
	if actual != expected {
		t.Fatalf("bad: actual:\n%s\n\nexpected:\n%s", actual, expected)
	}
}

func TestContext2Apply_multiDepose_createBeforeDestroy(t *testing.T) {
	m := testModule(t, "apply-multi-depose-create-before-destroy")
	p := testProvider("aws")
	ps := map[addrs.Provider]providers.Factory{addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p)}
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"require_new": {Type: cty.String, Optional: true},
					"id":          {Type: cty.String, Computed: true},
				},
			},
		},
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.web").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	p.DiffFn = func(info *InstanceInfo, s *InstanceState, rc *ResourceConfig) (*InstanceDiff, error) {
		if rc == nil {
			return &InstanceDiff{
				Destroy: true,
			}, nil
		}

		rn, _ := rc.Get("require_new")
		return &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{
				"id": {
					New:         hcl2shim.UnknownVariableValue,
					NewComputed: true,
					RequiresNew: true,
				},
				"require_new": {
					Old:         s.Attributes["require_new"],
					New:         rn.(string),
					RequiresNew: true,
				},
			},
		}, nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: ps,
		State:     state,
	})
	createdInstanceId := "bar"
	// Create works
	createFunc := func(is *InstanceState, id *InstanceDiff) (*InstanceState, error) {
		return &InstanceState{
			ID: createdInstanceId,
			Attributes: map[string]string{
				"require_new": id.Attributes["require_new"].New,
			},
		}, nil
	}
	// Destroy starts broken
	destroyFunc := func(is *InstanceState) (*InstanceState, error) {
		return is, fmt.Errorf("destroy failed")
	}
	p.ApplyFn = func(info *InstanceInfo, is *InstanceState, id *InstanceDiff) (*InstanceState, error) {
		if id.Destroy {
			return destroyFunc(is)
		} else {
			return createFunc(is, id)
		}
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	// Destroy is broken, so even though CBD successfully replaces the instance,
	// we'll have to save the Deposed instance to destroy later
	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}

	checkStateString(t, state, `
aws_instance.web: (1 deposed)
  ID = bar
  provider = provider["registry.terraform.io/hashicorp/aws"]
  require_new = yes
  Deposed ID 1 = foo
	`)

	createdInstanceId = "baz"
	ctx = testContext2(t, &ContextOpts{
		Config:    m,
		Providers: ps,
		State:     state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	// We're replacing the primary instance once again. Destroy is _still_
	// broken, so the Deposed list gets longer
	state, diags = ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}

	// For this one we can't rely on checkStateString because its result is
	// not deterministic when multiple deposed objects are present. Instead,
	// we will probe the state object directly.
	{
		is := state.RootModule().Resources["aws_instance.web"].Instances[addrs.NoKey]
		t.Logf("aws_instance.web is %s", spew.Sdump(is))
		if is.Current == nil {
			t.Fatalf("no current object for aws_instance web; should have one")
		}
		if !bytes.Contains(is.Current.AttrsJSON, []byte("baz")) {
			t.Fatalf("incorrect current object attrs %s; want id=baz", is.Current.AttrsJSON)
		}
		if got, want := len(is.Deposed), 2; got != want {
			t.Fatalf("wrong number of deposed instances %d; want %d", got, want)
		}
		var foos, bars int
		for _, obj := range is.Deposed {
			if bytes.Contains(obj.AttrsJSON, []byte("foo")) {
				foos++
			}
			if bytes.Contains(obj.AttrsJSON, []byte("bar")) {
				bars++
			}
		}
		if got, want := foos, 1; got != want {
			t.Fatalf("wrong number of deposed instances with id=foo %d; want %d", got, want)
		}
		if got, want := bars, 1; got != want {
			t.Fatalf("wrong number of deposed instances with id=bar %d; want %d", got, want)
		}
	}

	// Destroy partially fixed!
	destroyFunc = func(is *InstanceState) (*InstanceState, error) {
		if is.ID == "foo" || is.ID == "baz" {
			return nil, nil
		} else {
			return is, fmt.Errorf("destroy partially failed")
		}
	}

	createdInstanceId = "qux"
	ctx = testContext2(t, &ContextOpts{
		Config:    m,
		Providers: ps,
		State:     state,
	})
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}
	state, diags = ctx.Apply()
	// Expect error because 1/2 of Deposed destroys failed
	if diags == nil {
		t.Fatal("should have error")
	}

	// foo and baz are now gone, bar sticks around
	checkStateString(t, state, `
aws_instance.web: (1 deposed)
  ID = qux
  provider = provider["registry.terraform.io/hashicorp/aws"]
  require_new = yes
  Deposed ID 1 = bar
	`)

	// Destroy working fully!
	destroyFunc = func(is *InstanceState) (*InstanceState, error) {
		return nil, nil
	}

	createdInstanceId = "quux"
	ctx = testContext2(t, &ContextOpts{
		Config:    m,
		Providers: ps,
		State:     state,
	})
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}
	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatal("should not have error:", diags.Err())
	}

	// And finally the state is clean
	checkStateString(t, state, `
aws_instance.web:
  ID = quux
  provider = provider["registry.terraform.io/hashicorp/aws"]
  require_new = yes
	`)
}

// Verify that a normal provisioner with on_failure "continue" set won't
// taint the resource and continues executing.
func TestContext2Apply_provisionerFailContinue(t *testing.T) {
	m := testModule(t, "apply-provisioner-fail-continue")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		return fmt.Errorf("provisioner error")
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = bar
  type = aws_instance
  `)

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

// Verify that a normal provisioner with on_failure "continue" records
// the error with the hook.
func TestContext2Apply_provisionerFailContinueHook(t *testing.T) {
	h := new(MockHook)
	m := testModule(t, "apply-provisioner-fail-continue")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		return fmt.Errorf("provisioner error")
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	if !h.PostProvisionInstanceStepCalled {
		t.Fatal("PostProvisionInstanceStep not called")
	}
	if h.PostProvisionInstanceStepErrorArg == nil {
		t.Fatal("should have error")
	}
}

func TestContext2Apply_provisionerDestroy(t *testing.T) {
	m := testModule(t, "apply-provisioner-destroy")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok || val != "destroy a bar" {
			t.Fatalf("bad value for foo: %v %#v", val, c)
		}

		return nil
	}

	state := states.NewState()
	root := state.RootModule()
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr(`aws_instance.foo["a"]`).Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","foo":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:  m,
		State:   state,
		Destroy: true,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `<no state>`)

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

// Verify that on destroy provisioner failure, nothing happens to the instance
func TestContext2Apply_provisionerDestroyFail(t *testing.T) {
	m := testModule(t, "apply-provisioner-destroy")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		return fmt.Errorf("provisioner error")
	}

	state := states.NewState()
	root := state.RootModule()
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr(`aws_instance.foo["a"]`).Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","foo":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:  m,
		State:   state,
		Destroy: true,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should error")
	}

	checkStateString(t, state, `
aws_instance.foo["a"]:
  ID = bar
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = bar
	`)

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

// Verify that on destroy provisioner failure with "continue" that
// we continue to the next provisioner.
func TestContext2Apply_provisionerDestroyFailContinue(t *testing.T) {
	m := testModule(t, "apply-provisioner-destroy-continue")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var l sync.Mutex
	var calls []string
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok {
			t.Fatalf("bad value for foo: %v %#v", val, c)
		}

		l.Lock()
		defer l.Unlock()
		calls = append(calls, val.(string))
		return fmt.Errorf("provisioner error")
	}

	state := states.NewState()
	root := state.RootModule()
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr(`aws_instance.foo["a"]`).Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:  m,
		State:   state,
		Destroy: true,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `<no state>`)

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}

	expected := []string{"one", "two"}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("wrong commands\ngot:  %#v\nwant: %#v", calls, expected)
	}
}

// Verify that on destroy provisioner failure with "continue" that
// we continue to the next provisioner. But if the next provisioner defines
// to fail, then we fail after running it.
func TestContext2Apply_provisionerDestroyFailContinueFail(t *testing.T) {
	m := testModule(t, "apply-provisioner-destroy-fail")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var l sync.Mutex
	var calls []string
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok {
			t.Fatalf("bad value for foo: %v %#v", val, c)
		}

		l.Lock()
		defer l.Unlock()
		calls = append(calls, val.(string))
		return fmt.Errorf("provisioner error")
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:  m,
		State:   state,
		Destroy: true,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("apply succeeded; wanted error from second provisioner")
	}

	checkStateString(t, state, `
aws_instance.foo:
  ID = bar
  provider = provider["registry.terraform.io/hashicorp/aws"]
  `)

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}

	expected := []string{"one", "two"}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("bad: %#v", calls)
	}
}

// Verify destroy provisioners are not run for tainted instances.
func TestContext2Apply_provisionerDestroyTainted(t *testing.T) {
	m := testModule(t, "apply-provisioner-destroy")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	destroyCalled := false
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		expected := "create a b"
		if rs.ID == "bar" {
			destroyCalled = true
			return nil
		}

		val, ok := c.Config["command"]
		if !ok || val != expected {
			t.Fatalf("bad value for command: %v %#v", val, c)
		}

		return nil
	}

	state := states.NewState()
	root := state.RootModule()
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr(`aws_instance.foo["a"]`).Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
		Variables: InputValues{
			"input": &InputValue{
				Value: cty.MapVal(map[string]cty.Value{
					"a": cty.StringVal("b"),
				}),
				SourceType: ValueFromInput,
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.foo["a"]:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = bar
  type = aws_instance
	`)

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}

	if destroyCalled {
		t.Fatal("destroy should not be called")
	}
}

func TestContext2Apply_provisionerResourceRef(t *testing.T) {
	m := testModule(t, "apply-provisioner-resource-ref")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr := testProvisioner()
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok || val != "2" {
			t.Fatalf("bad value for foo: %v %#v", val, c)
		}

		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerResourceRefStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

func TestContext2Apply_provisionerSelfRef(t *testing.T) {
	m := testModule(t, "apply-provisioner-self-ref")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok || val != "bar" {
			t.Fatalf("bad value for command: %v %#v", val, c)
		}

		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerSelfRefStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}
}

func TestContext2Apply_provisionerMultiSelfRef(t *testing.T) {
	var lock sync.Mutex
	commands := make([]string, 0, 5)

	m := testModule(t, "apply-provisioner-multi-self-ref")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		lock.Lock()
		defer lock.Unlock()

		val, ok := c.Config["command"]
		if !ok {
			t.Fatalf("bad value for command: %v %#v", val, c)
		}

		commands = append(commands, val.(string))
		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerMultiSelfRefStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}

	// Verify our result
	sort.Strings(commands)
	expectedCommands := []string{"number 0", "number 1", "number 2"}
	if !reflect.DeepEqual(commands, expectedCommands) {
		t.Fatalf("bad: %#v", commands)
	}
}

func TestContext2Apply_provisionerMultiSelfRefSingle(t *testing.T) {
	var lock sync.Mutex
	order := make([]string, 0, 5)

	m := testModule(t, "apply-provisioner-multi-self-ref-single")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		lock.Lock()
		defer lock.Unlock()

		val, ok := c.Config["order"]
		if !ok {
			t.Fatalf("bad value for order: %v %#v", val, c)
		}

		order = append(order, val.(string))
		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerMultiSelfRefSingleStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner not invoked")
	}

	// Verify our result
	sort.Strings(order)
	expectedOrder := []string{"0", "1", "2"}
	if !reflect.DeepEqual(order, expectedOrder) {
		t.Fatalf("bad: %#v", order)
	}
}

func TestContext2Apply_provisionerExplicitSelfRef(t *testing.T) {
	m := testModule(t, "apply-provisioner-explicit-self-ref")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok || val != "bar" {
			t.Fatalf("bad value for command: %v %#v", val, c)
		}

		return nil
	}

	var state *states.State
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			Provisioners: map[string]ProvisionerFactory{
				"shell": testProvisionerFuncFixed(pr),
			},
		})

		_, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		// Verify apply was invoked
		if !pr.ProvisionResourceCalled {
			t.Fatalf("provisioner not invoked")
		}
	}

	{
		ctx := testContext2(t, &ContextOpts{
			Config:  m,
			Destroy: true,
			State:   state,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			Provisioners: map[string]ProvisionerFactory{
				"shell": testProvisionerFuncFixed(pr),
			},
		})

		_, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("diags: %s", diags.Err())
		}

		checkStateString(t, state, `<no state>`)
	}
}

func TestContext2Apply_provisionerForEachSelfRef(t *testing.T) {
	m := testModule(t, "apply-provisioner-for-each-self")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		val, ok := c.Config["command"]
		if !ok {
			t.Fatalf("bad value for command: %v %#v", val, c)
		}

		return nil
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}
}

// Provisioner should NOT run on a diff, only create
func TestContext2Apply_Provisioner_Diff(t *testing.T) {
	m := testModule(t, "apply-provisioner-diff")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		return nil
	}
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("apply failed")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyProvisionerDiffStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was invoked
	if !pr.ProvisionResourceCalled {
		t.Fatalf("provisioner was not called on first apply")
	}
	pr.ProvisionResourceCalled = false

	// Change the state to force a diff
	mod := state.RootModule()
	obj := mod.Resources["aws_instance.bar"].Instances[addrs.NoKey].Current
	var attrs map[string]interface{}
	err := json.Unmarshal(obj.AttrsJSON, &attrs)
	if err != nil {
		t.Fatal(err)
	}
	attrs["foo"] = "baz"
	obj.AttrsJSON, err = json.Marshal(attrs)
	if err != nil {
		t.Fatal(err)
	}

	// Re-create context with state
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	}

	state2, diags := ctx.Apply()
	if diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("apply failed")
	}

	actual = strings.TrimSpace(state2.String())
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Verify apply was NOT invoked
	if pr.ProvisionResourceCalled {
		t.Fatalf("provisioner was called on second apply; should not have been")
	}
}

func TestContext2Apply_outputDiffVars(t *testing.T) {
	m := testModule(t, "apply-good")
	p := testProvider("aws")

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.baz").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		if d.Destroy {
			return nil, nil
		}

		result := s.MergeDiff(d)
		result.ID = "foo"
		return result, nil
	}
	p.DiffFn = func(info *InstanceInfo, s *InstanceState, rc *ResourceConfig) (*InstanceDiff, error) {
		d := &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{},
		}
		if new, ok := rc.Get("value"); ok {
			d.Attributes["value"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		if new, ok := rc.Get("foo"); ok {
			d.Attributes["foo"] = &ResourceAttrDiff{
				New: new.(string),
			}
		} else if rc.IsComputed("foo") {
			d.Attributes["foo"] = &ResourceAttrDiff{
				NewComputed: true,
				Type:        DiffAttrOutput, // This doesn't actually really do anything anymore, but this test originally set it.
			}
		}
		if new, ok := rc.Get("num"); ok {
			d.Attributes["num"] = &ResourceAttrDiff{
				New: fmt.Sprintf("%#v", new),
			}
		}
		return d, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	}
	if _, diags := ctx.Apply(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("apply failed")
	}
}

func TestContext2Apply_destroyX(t *testing.T) {
	m := testModule(t, "apply-destroy")
	h := new(HookRecordApplyOrder)
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	// First plan and apply a create operation
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Next, plan and apply a destroy operation
	h.Active = true
	ctx = testContext2(t, &ContextOpts{
		Destroy: true,
		State:   state,
		Config:  m,
		Hooks:   []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Test that things were destroyed
	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyDestroyStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Test that things were destroyed _in the right order_
	expected2 := []string{"aws_instance.bar", "aws_instance.foo"}
	actual2 := h.IDs
	if !reflect.DeepEqual(actual2, expected2) {
		t.Fatalf("expected: %#v\n\ngot:%#v", expected2, actual2)
	}
}

func TestContext2Apply_destroyOrder(t *testing.T) {
	m := testModule(t, "apply-destroy")
	h := new(HookRecordApplyOrder)
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	// First plan and apply a create operation
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	t.Logf("State 1: %s", state)

	// Next, plan and apply a destroy
	h.Active = true
	ctx = testContext2(t, &ContextOpts{
		Destroy: true,
		State:   state,
		Config:  m,
		Hooks:   []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Test that things were destroyed
	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyDestroyStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}

	// Test that things were destroyed _in the right order_
	expected2 := []string{"aws_instance.bar", "aws_instance.foo"}
	actual2 := h.IDs
	if !reflect.DeepEqual(actual2, expected2) {
		t.Fatalf("expected: %#v\n\ngot:%#v", expected2, actual2)
	}
}

// https://github.com/hashicorp/terraform/issues/2767
func TestContext2Apply_destroyModulePrefix(t *testing.T) {
	m := testModule(t, "apply-destroy-module-resource-prefix")
	h := new(MockHook)
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	// First plan and apply a create operation
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Verify that we got the apply info correct
	if v := h.PreApplyAddr.String(); v != "module.child.aws_instance.foo" {
		t.Fatalf("bad: %s", v)
	}

	// Next, plan and apply a destroy operation and reset the hook
	h = new(MockHook)
	ctx = testContext2(t, &ContextOpts{
		Destroy: true,
		State:   state,
		Config:  m,
		Hooks:   []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Test that things were destroyed
	if v := h.PreApplyAddr.String(); v != "module.child.aws_instance.foo" {
		t.Fatalf("bad: %s", v)
	}
}

func TestContext2Apply_destroyNestedModule(t *testing.T) {
	m := testModule(t, "apply-destroy-nested-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	// First plan and apply a create operation
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Test that things were destroyed
	actual := strings.TrimSpace(s.String())
	if actual != "<no state>" {
		t.Fatalf("expected no state, got: %s", actual)
	}
}

func TestContext2Apply_destroyDeeplyNestedModule(t *testing.T) {
	m := testModule(t, "apply-destroy-deeply-nested-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	// First plan and apply a create operation
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Test that things were destroyed
	if !s.Empty() {
		t.Fatalf("wrong final state %s\nwant empty state", spew.Sdump(s))
	}
}

// https://github.com/hashicorp/terraform/issues/5440
func TestContext2Apply_destroyModuleWithAttrsReferencingResource(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-destroy-module-with-attrs")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var state *states.State
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		if p, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("plan diags: %s", diags.Err())
		} else {
			t.Logf("Step 1 plan: %s", legacyDiffComparisonString(p.Changes))
		}

		var diags tfdiags.Diagnostics
		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("apply errs: %s", diags.Err())
		}

		t.Logf("Step 1 state: %s", state)
	}

	h := new(HookRecordApplyOrder)
	h.Active = true

	{
		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			Config:  m,
			State:   state,
			Hooks:   []Hook{h},
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		plan, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("destroy plan err: %s", diags.Err())
		}

		t.Logf("Step 2 plan: %s", legacyDiffComparisonString(plan.Changes))

		ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
		if err != nil {
			t.Fatalf("failed to round-trip through planfile: %s", err)
		}

		ctxOpts.Providers = map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}

		ctx, diags = NewContext(ctxOpts)
		if diags.HasErrors() {
			t.Fatalf("err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("destroy apply err: %s", diags.Err())
		}

		t.Logf("Step 2 state: %s", state)
	}

	//Test that things were destroyed
	if state.HasResources() {
		t.Fatal("expected empty state, got:", state)
	}
}

func TestContext2Apply_destroyWithModuleVariableAndCount(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-destroy-mod-var-and-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var state *states.State
	var diags tfdiags.Diagnostics
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("plan err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("apply err: %s", diags.Err())
		}
	}

	h := new(HookRecordApplyOrder)
	h.Active = true

	{
		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			Config:  m,
			State:   state,
			Hooks:   []Hook{h},
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		plan, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("destroy plan err: %s", diags.Err())
		}

		ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
		if err != nil {
			t.Fatalf("failed to round-trip through planfile: %s", err)
		}

		ctxOpts.Providers =
			map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			}

		ctx, diags = NewContext(ctxOpts)
		if diags.HasErrors() {
			t.Fatalf("err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("destroy apply err: %s", diags.Err())
		}
	}

	//Test that things were destroyed
	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`
<no state>`)
	if actual != expected {
		t.Fatalf("expected: \n%s\n\nbad: \n%s", expected, actual)
	}
}

func TestContext2Apply_destroyTargetWithModuleVariableAndCount(t *testing.T) {
	m := testModule(t, "apply-destroy-mod-var-and-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var state *states.State
	var diags tfdiags.Diagnostics
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("plan err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("apply err: %s", diags.Err())
		}
	}

	{
		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			Config:  m,
			State:   state,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			Targets: []addrs.Targetable{
				addrs.RootModuleInstance.Child("child", addrs.NoKey),
			},
		})

		_, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("plan err: %s", diags)
		}
		if len(diags) != 1 {
			// Should have one warning that -target is in effect.
			t.Fatalf("got %d diagnostics in plan; want 1", len(diags))
		}
		if got, want := diags[0].Severity(), tfdiags.Warning; got != want {
			t.Errorf("wrong diagnostic severity %#v; want %#v", got, want)
		}
		if got, want := diags[0].Description().Summary, "Resource targeting is in effect"; got != want {
			t.Errorf("wrong diagnostic summary %#v; want %#v", got, want)
		}

		// Destroy, targeting the module explicitly
		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("destroy apply err: %s", diags)
		}
		if len(diags) != 1 {
			t.Fatalf("got %d diagnostics; want 1", len(diags))
		}
		if got, want := diags[0].Severity(), tfdiags.Warning; got != want {
			t.Errorf("wrong diagnostic severity %#v; want %#v", got, want)
		}
		if got, want := diags[0].Description().Summary, "Applied changes may be incomplete"; got != want {
			t.Errorf("wrong diagnostic summary %#v; want %#v", got, want)
		}
	}

	//Test that things were destroyed
	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`<no state>`)
	if actual != expected {
		t.Fatalf("expected: \n%s\n\nbad: \n%s", expected, actual)
	}
}

func TestContext2Apply_destroyWithModuleVariableAndCountNested(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-destroy-mod-var-and-count-nested")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var state *states.State
	var diags tfdiags.Diagnostics
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("plan err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("apply err: %s", diags.Err())
		}
	}

	h := new(HookRecordApplyOrder)
	h.Active = true

	{
		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			Config:  m,
			State:   state,
			Hooks:   []Hook{h},
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		plan, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("destroy plan err: %s", diags.Err())
		}

		ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
		if err != nil {
			t.Fatalf("failed to round-trip through planfile: %s", err)
		}

		ctxOpts.Providers =
			map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			}

		ctx, diags = NewContext(ctxOpts)
		if diags.HasErrors() {
			t.Fatalf("err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("destroy apply err: %s", diags.Err())
		}
	}

	//Test that things were destroyed
	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`
<no state>`)
	if actual != expected {
		t.Fatalf("expected: \n%s\n\nbad: \n%s", expected, actual)
	}
}

func TestContext2Apply_destroyOutputs(t *testing.T) {
	m := testModule(t, "apply-destroy-outputs")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
		// add the required id
		m := req.Config.AsValueMap()
		m["id"] = cty.StringVal("foo")

		return providers.ReadDataSourceResponse{
			State: cty.ObjectVal(m),
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	// First plan and apply a create operation
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()

	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// Next, plan and apply a destroy operation
	ctx = testContext2(t, &ContextOpts{
		Destroy: true,
		State:   state,
		Config:  m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) > 0 {
		t.Fatalf("expected no resources, got: %#v", mod)
	}

	// destroying again should produce no errors
	ctx = testContext2(t, &ContextOpts{
		Destroy: true,
		State:   state,
		Config:  m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatal(diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatal(diags.Err())
	}
}

func TestContext2Apply_destroyOrphan(t *testing.T) {
	m := testModule(t, "apply-error")
	p := testProvider("aws")
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.baz").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		if d.Destroy {
			return nil, nil
		}

		result := s.MergeDiff(d)
		result.ID = "foo"
		return result, nil
	}
	p.DiffFn = func(info *InstanceInfo, s *InstanceState, rc *ResourceConfig) (*InstanceDiff, error) {
		d := &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{},
		}
		if new, ok := rc.Get("value"); ok {
			d.Attributes["value"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		if new, ok := rc.Get("foo"); ok {
			d.Attributes["foo"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		return d, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := s.RootModule()
	if _, ok := mod.Resources["aws_instance.baz"]; ok {
		t.Fatalf("bad: %#v", mod.Resources)
	}
}

func TestContext2Apply_destroyTaintedProvisioner(t *testing.T) {
	m := testModule(t, "apply-destroy-provisioner")
	p := testProvider("aws")
	pr := testProvisioner()
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	called := false
	pr.ApplyFn = func(rs *InstanceState, c *ResourceConfig) error {
		called = true
		return nil
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Provisioners: map[string]ProvisionerFactory{
			"shell": testProvisionerFuncFixed(pr),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if called {
		t.Fatal("provisioner should not be called")
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace("<no state>")
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_error(t *testing.T) {
	errored := false

	m := testModule(t, "apply-error")
	p := testProvider("aws")
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	p.ApplyFn = func(*InstanceInfo, *InstanceState, *InstanceDiff) (*InstanceState, error) {
		if errored {
			state := &InstanceState{
				ID: "bar",
			}
			return state, fmt.Errorf("error")
		}
		errored = true

		return &InstanceState{
			ID: "foo",
			Attributes: map[string]string{
				"value": "2",
			},
		}, nil
	}
	p.DiffFn = func(info *InstanceInfo, s *InstanceState, rc *ResourceConfig) (*InstanceDiff, error) {
		d := &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{},
		}
		if new, ok := rc.Get("value"); ok {
			d.Attributes["value"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		if new, ok := rc.Get("foo"); ok {
			d.Attributes["foo"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		return d, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyErrorStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}
}

func TestContext2Apply_errorDestroy(t *testing.T) {
	m := testModule(t, "empty")
	p := testProvider("test")

	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_thing": {
				Attributes: map[string]*configschema.Attribute{
					"id": {Type: cty.String, Optional: true},
				},
			},
		},
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		// Should actually be called for this test, because Terraform Core
		// constructs the plan for a destroy operation itself.
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		// The apply (in this case, a destroy) always fails, so we can verify
		// that the object stays in the state after a destroy fails even though
		// we aren't returning a new state object here.
		return providers.ApplyResourceChangeResponse{
			Diagnostics: tfdiags.Diagnostics(nil).Append(fmt.Errorf("failed")),
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State: states.BuildState(func(ss *states.SyncState) {
			ss.SetResourceInstanceCurrent(
				addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test_thing",
					Name: "foo",
				}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
				&states.ResourceInstanceObjectSrc{
					Status:    states.ObjectReady,
					AttrsJSON: []byte(`{"id":"baz"}`),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				},
			)
		}),
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`
test_thing.foo:
  ID = baz
  provider = provider["registry.terraform.io/hashicorp/test"]
`) // test_thing.foo is still here, even though provider returned no new state along with its error
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}
}

func TestContext2Apply_errorCreateInvalidNew(t *testing.T) {
	m := testModule(t, "apply-error")

	p := testProvider("aws")
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"value": {Type: cty.String, Optional: true},
					"foo":   {Type: cty.String, Optional: true},
				},
			},
		},
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		// We're intentionally returning an inconsistent new state here
		// because we want to test that Terraform ignores the inconsistency
		// when accompanied by another error.
		return providers.ApplyResourceChangeResponse{
			NewState: cty.ObjectVal(map[string]cty.Value{
				"value": cty.StringVal("wrong wrong wrong wrong"),
				"foo":   cty.StringVal("absolutely brimming over with wrongability"),
			}),
			Diagnostics: tfdiags.Diagnostics(nil).Append(fmt.Errorf("forced error")),
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}
	if got, want := len(diags), 1; got != want {
		// There should be no additional diagnostics generated by Terraform's own eval logic,
		// because the provider's own error supersedes them.
		t.Errorf("wrong number of diagnostics %d; want %d\n%s", got, want, diags.Err())
	}
	if got, want := diags.Err().Error(), "forced error"; !strings.Contains(got, want) {
		t.Errorf("returned error does not contain %q, but it should\n%s", want, diags.Err())
	}
	if got, want := len(state.RootModule().Resources), 2; got != want {
		t.Errorf("%d resources in state before prune; should have %d\n%s", got, want, spew.Sdump(state))
	}
	state.PruneResourceHusks() // aws_instance.bar with no instances gets left behind when we bail out, but that's okay
	if got, want := len(state.RootModule().Resources), 1; got != want {
		t.Errorf("%d resources in state after prune; should have only one (aws_instance.foo, tainted)\n%s", got, spew.Sdump(state))
	}
}

func TestContext2Apply_errorUpdateNullNew(t *testing.T) {
	m := testModule(t, "apply-error")

	p := testProvider("aws")
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"aws_instance": {
				Attributes: map[string]*configschema.Attribute{
					"value": {Type: cty.String, Optional: true},
					"foo":   {Type: cty.String, Optional: true},
				},
			},
		},
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		// We're intentionally returning no NewState here because we want to
		// test that Terraform retains the prior state, rather than treating
		// the returned null as "no state" (object deleted).
		return providers.ApplyResourceChangeResponse{
			Diagnostics: tfdiags.Diagnostics(nil).Append(fmt.Errorf("forced error")),
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State: states.BuildState(func(ss *states.SyncState) {
			ss.SetResourceInstanceCurrent(
				addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "aws_instance",
					Name: "foo",
				}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
				&states.ResourceInstanceObjectSrc{
					Status:    states.ObjectReady,
					AttrsJSON: []byte(`{"value":"old"}`),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("aws"),
					Module:   addrs.RootModule,
				},
			)
		}),
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}
	if got, want := len(diags), 1; got != want {
		// There should be no additional diagnostics generated by Terraform's own eval logic,
		// because the provider's own error supersedes them.
		t.Errorf("wrong number of diagnostics %d; want %d\n%s", got, want, diags.Err())
	}
	if got, want := diags.Err().Error(), "forced error"; !strings.Contains(got, want) {
		t.Errorf("returned error does not contain %q, but it should\n%s", want, diags.Err())
	}
	state.PruneResourceHusks()
	if got, want := len(state.RootModule().Resources), 1; got != want {
		t.Fatalf("%d resources in state; should have only one (aws_instance.foo, unmodified)\n%s", got, spew.Sdump(state))
	}

	is := state.ResourceInstance(addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "aws_instance",
		Name: "foo",
	}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance))
	if is == nil {
		t.Fatalf("aws_instance.foo is not in the state after apply")
	}
	if got, want := is.Current.AttrsJSON, []byte(`"old"`); !bytes.Contains(got, want) {
		t.Fatalf("incorrect attributes for aws_instance.foo\ngot: %s\nwant: JSON containing %s\n\n%s", got, want, spew.Sdump(is))
	}
}

func TestContext2Apply_errorPartial(t *testing.T) {
	errored := false

	m := testModule(t, "apply-error")
	p := testProvider("aws")

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		if errored {
			return s, fmt.Errorf("error")
		}
		errored = true

		return &InstanceState{
			ID: "foo",
			Attributes: map[string]string{
				"value": "2",
			},
		}, nil
	}
	p.DiffFn = func(info *InstanceInfo, s *InstanceState, rc *ResourceConfig) (*InstanceDiff, error) {
		d := &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{},
		}
		if new, ok := rc.Get("value"); ok {
			d.Attributes["value"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		if new, ok := rc.Get("foo"); ok {
			d.Attributes["foo"] = &ResourceAttrDiff{
				New: new.(string),
			}
		}
		return d, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags == nil {
		t.Fatal("should have error")
	}

	mod := s.RootModule()
	if len(mod.Resources) != 2 {
		t.Fatalf("bad: %#v", mod.Resources)
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyErrorPartialStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}
}

func TestContext2Apply_hook(t *testing.T) {
	m := testModule(t, "apply-good")
	h := new(MockHook)
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	if !h.PreApplyCalled {
		t.Fatal("should be called")
	}
	if !h.PostApplyCalled {
		t.Fatal("should be called")
	}
	if !h.PostStateUpdateCalled {
		t.Fatalf("should call post state update")
	}
}

func TestContext2Apply_hookOrphan(t *testing.T) {
	m := testModule(t, "apply-blank")
	h := new(MockHook)
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	if !h.PreApplyCalled {
		t.Fatal("should be called")
	}
	if !h.PostApplyCalled {
		t.Fatal("should be called")
	}
	if !h.PostStateUpdateCalled {
		t.Fatalf("should call post state update")
	}
}

func TestContext2Apply_idAttr(t *testing.T) {
	m := testModule(t, "apply-idattr")
	p := testProvider("aws")
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		result := s.MergeDiff(d)
		result.ID = "foo"
		result.Attributes = map[string]string{
			"id":  "bar",
			"num": "42",
		}

		return result, nil
	}
	p.DiffFn = func(*InstanceInfo, *InstanceState, *ResourceConfig) (*InstanceDiff, error) {
		return &InstanceDiff{
			Attributes: map[string]*ResourceAttrDiff{
				"num": &ResourceAttrDiff{
					New: "42",
				},
			},
		}, nil
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	mod := state.RootModule()
	rs, ok := mod.Resources["aws_instance.foo"]
	if !ok {
		t.Fatal("not in state")
	}
	var attrs map[string]interface{}
	err := json.Unmarshal(rs.Instances[addrs.NoKey].Current.AttrsJSON, &attrs)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := attrs["id"], "foo"; got != want {
		t.Fatalf("wrong id\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestContext2Apply_outputBasic(t *testing.T) {
	m := testModule(t, "apply-output")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyOutputStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_outputAdd(t *testing.T) {
	m1 := testModule(t, "apply-output-add-before")
	p1 := testProvider("aws")
	p1.ApplyFn = testApplyFn
	p1.DiffFn = testDiffFn
	ctx1 := testContext2(t, &ContextOpts{
		Config: m1,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p1),
		},
	})

	if _, diags := ctx1.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	state1, diags := ctx1.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	m2 := testModule(t, "apply-output-add-after")
	p2 := testProvider("aws")
	p2.ApplyFn = testApplyFn
	p2.DiffFn = testDiffFn
	ctx2 := testContext2(t, &ContextOpts{
		Config: m2,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p2),
		},

		State: state1,
	})

	if _, diags := ctx2.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	state2, diags := ctx2.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state2.String())
	expected := strings.TrimSpace(testTerraformApplyOutputAddStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_outputList(t *testing.T) {
	m := testModule(t, "apply-output-list")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyOutputListStr)
	if actual != expected {
		t.Fatalf("expected: \n%s\n\nbad: \n%s", expected, actual)
	}
}

func TestContext2Apply_outputMulti(t *testing.T) {
	m := testModule(t, "apply-output-multi")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyOutputMultiStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_outputMultiIndex(t *testing.T) {
	m := testModule(t, "apply-output-multi-index")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyOutputMultiIndexStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_taintX(t *testing.T) {
	m := testModule(t, "apply-taint")
	p := testProvider("aws")
	// destroyCount tests against regression of
	// https://github.com/hashicorp/terraform/issues/1056
	var destroyCount = int32(0)
	var once sync.Once
	simulateProviderDelay := func() {
		time.Sleep(10 * time.Millisecond)
	}

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		once.Do(simulateProviderDelay)
		if d.Destroy {
			atomic.AddInt32(&destroyCount, 1)
		}
		return testApplyFn(info, s, d)
	}
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"baz","num": "2", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf("plan: %s", legacyDiffComparisonString(p.Changes))
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyTaintStr)
	if actual != expected {
		t.Fatalf("bad:\n%s", actual)
	}

	if destroyCount != 1 {
		t.Fatalf("Expected 1 destroy, got %d", destroyCount)
	}
}

func TestContext2Apply_taintDep(t *testing.T) {
	m := testModule(t, "apply-taint-dep")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"baz","num": "2", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"bar","num": "2", "type": "aws_instance", "foo": "baz"}`),
			Dependencies: []addrs.ConfigResource{mustConfigResourceAddr("aws_instance.foo")},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf("plan: %s", legacyDiffComparisonString(p.Changes))
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyTaintDepStr)
	if actual != expected {
		t.Fatalf("bad:\n%s", actual)
	}
}

func TestContext2Apply_taintDepRequiresNew(t *testing.T) {
	m := testModule(t, "apply-taint-dep-requires-new")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"baz","num": "2", "type": "aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"bar","num": "2", "type": "aws_instance", "foo": "baz"}`),
			Dependencies: []addrs.ConfigResource{mustConfigResourceAddr("aws_instance.foo")},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf("plan: %s", legacyDiffComparisonString(p.Changes))
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(testTerraformApplyTaintDepRequireNewStr)
	if actual != expected {
		t.Fatalf("bad:\n%s", actual)
	}
}

func TestContext2Apply_targeted(t *testing.T) {
	m := testModule(t, "apply-targeted")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) != 1 {
		t.Fatalf("expected 1 resource, got: %#v", mod.Resources)
	}

	checkStateString(t, state, `
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  num = 2
  type = aws_instance
	`)
}

func TestContext2Apply_targetedCount(t *testing.T) {
	m := testModule(t, "apply-targeted-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.foo.0:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.foo.1:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.foo.2:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
	`)
}

func TestContext2Apply_targetedCountIndex(t *testing.T) {
	m := testModule(t, "apply-targeted-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.ResourceInstance(
				addrs.ManagedResourceMode, "aws_instance", "foo", addrs.IntKey(1),
			),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.foo.1:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
	`)
}

func TestContext2Apply_targetedDestroy(t *testing.T) {
	m := testModule(t, "destroy-targeted")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetOutputValue("out", cty.StringVal("bar"), false)

	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.b").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-bcd345"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "a",
			),
		},
		Destroy: true,
	})

	if diags := ctx.Validate(); diags.HasErrors() {
		t.Fatalf("validate errors: %s", diags.Err())
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) != 0 {
		t.Fatalf("expected 0 resources, got: %#v", mod.Resources)
	}

	// the root output should not get removed; only the targeted resource.
	//
	// Note: earlier versions of this test expected 0 outputs, but it turns out
	// that was because Validate - not apply or destroy - removed the output
	// (which depends on the targeted resource) from state. That version of this
	// test did not match actual terraform behavior: the output remains in
	// state.
	//
	// TODO: Future refactoring may enable us to remove the output from state in
	// this case, and that would be Just Fine - this test can be modified to
	// expect 0 outputs.
	if len(mod.OutputValues) != 1 {
		t.Fatalf("expected 1 outputs, got: %#v", mod.OutputValues)
	}

	// the module instance should remain
	mod = state.Module(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	if len(mod.Resources) != 1 {
		t.Fatalf("expected 1 resources, got: %#v", mod.Resources)
	}
}

func TestContext2Apply_targetedDestroyCountDeps(t *testing.T) {
	m := testModule(t, "apply-destroy-targeted-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-bcd345"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:       states.ObjectReady,
			AttrsJSON:    []byte(`{"id":"i-abc123"}`),
			Dependencies: []addrs.ConfigResource{mustConfigResourceAddr("aws_instance.foo")},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `<no state>`)
}

// https://github.com/hashicorp/terraform/issues/4462
func TestContext2Apply_targetedDestroyModule(t *testing.T) {
	m := testModule(t, "apply-targeted-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-bcd345"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-abc123"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	child := state.EnsureModule(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-bcd345"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-abc123"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Child("child", addrs.NoKey).Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.bar:
  ID = i-abc123
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.foo:
  ID = i-bcd345
  provider = provider["registry.terraform.io/hashicorp/aws"]

module.child:
  aws_instance.bar:
    ID = i-abc123
    provider = provider["registry.terraform.io/hashicorp/aws"]
	`)
}

func TestContext2Apply_targetedDestroyCountIndex(t *testing.T) {
	m := testModule(t, "apply-targeted-count")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	foo := &states.ResourceInstanceObjectSrc{
		Status:    states.ObjectReady,
		AttrsJSON: []byte(`{"id":"i-bcd345"}`),
	}
	bar := &states.ResourceInstanceObjectSrc{
		Status:    states.ObjectReady,
		AttrsJSON: []byte(`{"id":"i-abc123"}`),
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[0]").Resource,
		foo,
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[1]").Resource,
		foo,
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[2]").Resource,
		foo,
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar[0]").Resource,
		bar,
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar[1]").Resource,
		bar,
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar[2]").Resource,
		bar,
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.ResourceInstance(
				addrs.ManagedResourceMode, "aws_instance", "foo", addrs.IntKey(2),
			),
			addrs.RootModuleInstance.ResourceInstance(
				addrs.ManagedResourceMode, "aws_instance", "bar", addrs.IntKey(1),
			),
		},
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.bar.0:
  ID = i-abc123
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.bar.2:
  ID = i-abc123
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.foo.0:
  ID = i-bcd345
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.foo.1:
  ID = i-bcd345
  provider = provider["registry.terraform.io/hashicorp/aws"]
	`)
}

func TestContext2Apply_targetedModule(t *testing.T) {
	m := testModule(t, "apply-targeted-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Child("child", addrs.NoKey),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.Module(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	if mod == nil {
		t.Fatalf("no child module found in the state!\n\n%#v", state)
	}
	if len(mod.Resources) != 2 {
		t.Fatalf("expected 2 resources, got: %#v", mod.Resources)
	}

	checkStateString(t, state, `
<no state>
module.child:
  aws_instance.bar:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    num = 2
    type = aws_instance
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    num = 2
    type = aws_instance
	`)
}

// GH-1858
func TestContext2Apply_targetedModuleDep(t *testing.T) {
	m := testModule(t, "apply-targeted-module-dep")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf("Diff: %s", legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	checkStateString(t, state, `
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  foo = foo
  type = aws_instance

  Dependencies:
    module.child.aws_instance.mod

module.child:
  aws_instance.mod:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]

  Outputs:

  output = foo
	`)
}

// GH-10911 untargeted outputs should not be in the graph, and therefore
// not execute.
func TestContext2Apply_targetedModuleUnrelatedOutputs(t *testing.T) {
	m := testModule(t, "apply-targeted-module-unrelated-outputs")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	_ = state.EnsureModule(addrs.RootModuleInstance.Child("child2", addrs.NoKey))

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Child("child2", addrs.NoKey),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// - module.child1's instance_id output is dropped because we don't preserve
	//   non-root module outputs between runs (they can be recalculated from config)
	// - module.child2's instance_id is updated because its dependency is updated
	// - child2_id is updated because if its transitive dependency via module.child2
	checkStateString(t, s, `
<no state>
Outputs:

child2_id = foo

module.child2:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]

  Outputs:

  instance_id = foo
`)
}

func TestContext2Apply_targetedModuleResource(t *testing.T) {
	m := testModule(t, "apply-targeted-module-resource")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Child("child", addrs.NoKey).Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.Module(addrs.RootModuleInstance.Child("child", addrs.NoKey))
	if mod == nil || len(mod.Resources) != 1 {
		t.Fatalf("expected 1 resource, got: %#v", mod)
	}

	checkStateString(t, state, `
<no state>
module.child:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    num = 2
    type = aws_instance
	`)
}

func TestContext2Apply_targetedResourceOrphanModule(t *testing.T) {
	m := testModule(t, "apply-targeted-resource-orphan-module")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	child := state.EnsureModule(addrs.RootModuleInstance.Child("parent", addrs.NoKey))
	child.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.bar").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"type":"aws_instance"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "foo",
			),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_unknownAttribute(t *testing.T) {
	m := testModule(t, "apply-unknown")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if !diags.HasErrors() {
		t.Error("should error, because attribute 'unknown' is still unknown after apply")
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyUnknownAttrStr)
	if actual != expected {
		t.Fatalf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_unknownAttributeInterpolate(t *testing.T) {
	m := testModule(t, "apply-unknown-interpolate")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags == nil {
		t.Fatal("should error")
	}
}

func TestContext2Apply_vars(t *testing.T) {
	fixture := contextFixtureApplyVars(t)
	opts := fixture.ContextOpts()
	opts.Variables = InputValues{
		"foo": &InputValue{
			Value:      cty.StringVal("us-east-1"),
			SourceType: ValueFromCaller,
		},
		"test_list": &InputValue{
			Value: cty.ListVal([]cty.Value{
				cty.StringVal("Hello"),
				cty.StringVal("World"),
			}),
			SourceType: ValueFromCaller,
		},
		"test_map": &InputValue{
			Value: cty.MapVal(map[string]cty.Value{
				"Hello": cty.StringVal("World"),
				"Foo":   cty.StringVal("Bar"),
				"Baz":   cty.StringVal("Foo"),
			}),
			SourceType: ValueFromCaller,
		},
		"amis": &InputValue{
			Value: cty.MapVal(map[string]cty.Value{
				"us-east-1": cty.StringVal("override"),
			}),
			SourceType: ValueFromCaller,
		},
	}
	ctx := testContext2(t, opts)

	diags := ctx.Validate()
	if len(diags) != 0 {
		t.Fatalf("bad: %s", diags.ErrWithWarnings())
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	got := strings.TrimSpace(state.String())
	want := strings.TrimSpace(testTerraformApplyVarsStr)
	if got != want {
		t.Errorf("wrong result\n\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestContext2Apply_varsEnv(t *testing.T) {
	fixture := contextFixtureApplyVarsEnv(t)
	opts := fixture.ContextOpts()
	opts.Variables = InputValues{
		"string": &InputValue{
			Value:      cty.StringVal("baz"),
			SourceType: ValueFromEnvVar,
		},
		"list": &InputValue{
			Value: cty.ListVal([]cty.Value{
				cty.StringVal("Hello"),
				cty.StringVal("World"),
			}),
			SourceType: ValueFromEnvVar,
		},
		"map": &InputValue{
			Value: cty.MapVal(map[string]cty.Value{
				"Hello": cty.StringVal("World"),
				"Foo":   cty.StringVal("Bar"),
				"Baz":   cty.StringVal("Foo"),
			}),
			SourceType: ValueFromEnvVar,
		},
	}
	ctx := testContext2(t, opts)

	diags := ctx.Validate()
	if len(diags) != 0 {
		t.Fatalf("bad: %s", diags.ErrWithWarnings())
	}

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(testTerraformApplyVarsEnvStr)
	if actual != expected {
		t.Errorf("wrong result\n\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_createBefore_depends(t *testing.T) {
	m := testModule(t, "apply-depends-create-before")
	h := new(HookRecordApplyOrder)
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "web",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","require_new":"ami-old"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)

	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "lb",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"baz","instance":"bar"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "web",
					},
					Module: addrs.RootModule,
				},
			},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	} else {
		t.Logf("plan:\n%s", legacyDiffComparisonString(p.Changes))
	}

	h.Active = true
	state, diags := ctx.Apply()
	if diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("apply failed")
	}

	mod := state.RootModule()
	if len(mod.Resources) < 2 {
		t.Logf("state after apply:\n%s", state.String())
		t.Fatalf("only %d resources in root module; want at least 2", len(mod.Resources))
	}

	got := strings.TrimSpace(state.String())
	want := strings.TrimSpace(testTerraformApplyDependsCreateBeforeStr)
	if got != want {
		t.Fatalf("wrong final state\ngot:\n%s\n\nwant:\n%s", got, want)
	}

	// Test that things were managed _in the right order_
	order := h.States

	diffs := h.Diffs
	if !order[0].IsNull() || diffs[0].Action == plans.Delete {
		t.Fatalf("should create new instance first: %#v", order)
	}

	if order[1].GetAttr("id").AsString() != "baz" {
		t.Fatalf("update must happen after create: %#v", order[1])
	}

	if order[2].GetAttr("id").AsString() != "bar" || diffs[2].Action != plans.Delete {
		t.Fatalf("destroy must happen after update: %#v", order[2])
	}
}

func TestContext2Apply_singleDestroy(t *testing.T) {
	m := testModule(t, "apply-depends-create-before")
	h := new(HookRecordApplyOrder)
	p := testProvider("aws")
	invokeCount := 0
	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		invokeCount++
		switch invokeCount {
		case 1:
			if d.Destroy {
				t.Fatalf("should not destroy")
			}
			if s.ID != "" {
				t.Fatalf("should not have ID")
			}
		case 2:
			if d.Destroy {
				t.Fatalf("should not destroy")
			}
			if s.ID != "baz" {
				t.Fatalf("should have id")
			}
		case 3:
			if !d.Destroy {
				t.Fatalf("should destroy")
			}
			if s.ID == "" {
				t.Fatalf("should have ID")
			}
		default:
			t.Fatalf("bad invoke count %d", invokeCount)
		}
		return testApplyFn(info, s, d)
	}
	p.DiffFn = testDiffFn
	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "web",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar","require_new":"ami-old"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)

	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "aws_instance",
			Name: "lb",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"baz","instance":"bar"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "web",
					},
					Module: addrs.RootModule,
				},
			},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("aws"),
			Module:   addrs.RootModule,
		},
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Hooks:  []Hook{h},
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	h.Active = true
	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	if invokeCount != 3 {
		t.Fatalf("bad: %d", invokeCount)
	}
}

// GH-7824
func TestContext2Apply_issue7824(t *testing.T) {
	p := testProvider("template")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"template_file": {
				Attributes: map[string]*configschema.Attribute{
					"template":                {Type: cty.String, Optional: true},
					"__template_requires_new": {Type: cty.Bool, Optional: true},
				},
			},
		},
	}

	m, snap := testModuleWithSnapshot(t, "issue-7824")

	// Apply cleanly step 0
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("template"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	// Write / Read plan to simulate running it through a Plan file
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatalf("failed to round-trip through planfile: %s", err)
	}

	ctxOpts.Providers =
		map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("template"): testProviderFuncFixed(p),
		}

	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}
}

// This deals with the situation where a splat expression is used referring
// to another resource whose count is non-constant.
func TestContext2Apply_issue5254(t *testing.T) {
	// Create a provider. We use "template" here just to match the repro
	// we got from the issue itself.
	p := testProvider("template")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"template_file": {
				Attributes: map[string]*configschema.Attribute{
					"template":                {Type: cty.String, Optional: true},
					"__template_requires_new": {Type: cty.Bool, Optional: true},
					"id":                      {Type: cty.String, Computed: true},
					"type":                    {Type: cty.String, Computed: true},
				},
			},
		},
	}

	// Apply cleanly step 0
	ctx := testContext2(t, &ContextOpts{
		Config: testModule(t, "issue-5254/step-0"),
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("template"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	m, snap := testModuleWithSnapshot(t, "issue-5254/step-1")

	// Application success. Now make the modification and store a plan
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("template"): testProviderFuncFixed(p),
		},
	})

	plan, diags = ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	// Write / Read plan to simulate running it through a Plan file
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatalf("failed to round-trip through planfile: %s", err)
	}

	ctxOpts.Providers = map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("template"): testProviderFuncFixed(p),
	}

	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	actual := strings.TrimSpace(state.String())
	expected := strings.TrimSpace(`
template_file.child:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/template"]
  __template_requires_new = true
  template = Hi
  type = template_file

  Dependencies:
    template_file.parent
template_file.parent.0:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/template"]
  template = Hi
  type = template_file
`)
	if actual != expected {
		t.Fatalf("wrong final state\ngot:\n%s\n\nwant:\n%s", actual, expected)
	}
}

func TestContext2Apply_targetedWithTaintedInState(t *testing.T) {
	p := testProvider("aws")
	p.DiffFn = testDiffFn
	p.ApplyFn = testApplyFn
	m, snap := testModuleWithSnapshot(t, "apply-tainted-targets")

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.ifailedprovisioners").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"ifailedprovisioners"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Resource(
				addrs.ManagedResourceMode, "aws_instance", "iambeingadded",
			),
		},
		State: state,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	// Write / Read plan to simulate running it through a Plan file
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatalf("failed to round-trip through planfile: %s", err)
	}

	ctxOpts.Providers = map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
	}

	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(`
aws_instance.iambeingadded:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
aws_instance.ifailedprovisioners: (tainted)
  ID = ifailedprovisioners
  provider = provider["registry.terraform.io/hashicorp/aws"]
		`)
	if actual != expected {
		t.Fatalf("expected state: \n%s\ngot: \n%s", expected, actual)
	}
}

// Higher level test exposing the bug this covers in
// TestResource_ignoreChangesRequired
func TestContext2Apply_ignoreChangesCreate(t *testing.T) {
	m := testModule(t, "apply-ignore-changes-create")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	instanceSchema := p.GetSchemaReturn.ResourceTypes["aws_instance"]
	instanceSchema.Attributes["required_field"] = &configschema.Attribute{
		Type:     cty.String,
		Required: true,
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	mod := state.RootModule()
	if len(mod.Resources) != 1 {
		t.Fatalf("bad: %s", state)
	}

	actual := strings.TrimSpace(state.String())
	// Expect no changes from original state
	expected := strings.TrimSpace(`
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  required_field = set
  type = aws_instance
`)
	if actual != expected {
		t.Fatalf("expected:\n%s\ngot:\n%s", expected, actual)
	}
}

func TestContext2Apply_ignoreChangesWithDep(t *testing.T) {
	m := testModule(t, "apply-ignore-changes-dep")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn

	p.DiffFn = func(i *InstanceInfo, s *InstanceState, c *ResourceConfig) (*InstanceDiff, error) {
		switch i.Type {
		case "aws_instance":
			newAmi, _ := c.Get("ami")
			return &InstanceDiff{
				Attributes: map[string]*ResourceAttrDiff{
					"ami": &ResourceAttrDiff{
						Old:         s.Attributes["ami"],
						New:         newAmi.(string),
						RequiresNew: true,
					},
				},
			}, nil
		case "aws_eip":
			return testDiffFn(i, s, c)
		default:
			t.Fatalf("Unexpected type: %s", i.Type)
			return nil, nil
		}
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-abc123","ami":"ami-abcd1234"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"i-bcd234","ami":"i-bcd234"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_eip.foo[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"eip-abc123","instance":"i-abc123"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "foo",
					},
					Module: addrs.RootModule,
				},
			},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_eip.foo[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"eip-bcd234","instance":"i-bcd234"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "foo",
					},
					Module: addrs.RootModule,
				},
			},
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State: state.DeepCopy(),
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	s, diags := ctx.Apply()
	assertNoErrors(t, diags)

	actual := strings.TrimSpace(s.String())
	expected := strings.TrimSpace(state.String())
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}
}

func TestContext2Apply_ignoreChangesWildcard(t *testing.T) {
	m := testModule(t, "apply-ignore-changes-wildcard")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	instanceSchema := p.GetSchemaReturn.ResourceTypes["aws_instance"]
	instanceSchema.Attributes["required_field"] = &configschema.Attribute{
		Type:     cty.String,
		Required: true,
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if p, diags := ctx.Plan(); diags.HasErrors() {
		logDiagnostics(t, diags)
		t.Fatal("plan failed")
	} else {
		t.Logf(legacyDiffComparisonString(p.Changes))
	}

	state, diags := ctx.Apply()
	assertNoErrors(t, diags)

	mod := state.RootModule()
	if len(mod.Resources) != 1 {
		t.Fatalf("bad: %s", state)
	}

	actual := strings.TrimSpace(state.String())
	// Expect no changes from original state
	expected := strings.TrimSpace(`
aws_instance.foo:
  ID = foo
  provider = provider["registry.terraform.io/hashicorp/aws"]
  required_field = set
  type = aws_instance
`)
	if actual != expected {
		t.Fatalf("expected:\n%s\ngot:\n%s", expected, actual)
	}
}

// https://github.com/hashicorp/terraform/issues/7378
func TestContext2Apply_destroyNestedModuleWithAttrsReferencingResource(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-destroy-nested-module-with-attrs")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	var state *states.State
	var diags tfdiags.Diagnostics
	{
		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
			},
		})

		// First plan and apply a create operation
		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("plan err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("apply err: %s", diags.Err())
		}
	}

	{
		ctx := testContext2(t, &ContextOpts{
			Destroy: true,
			Config:  m,
			State:   state,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
			},
		})

		plan, diags := ctx.Plan()
		if diags.HasErrors() {
			t.Fatalf("destroy plan err: %s", diags.Err())
		}

		ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
		if err != nil {
			t.Fatalf("failed to round-trip through planfile: %s", err)
		}

		ctxOpts.Providers = map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		}

		ctx, diags = NewContext(ctxOpts)
		if diags.HasErrors() {
			t.Fatalf("err: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("destroy apply err: %s", diags.Err())
		}
	}

	if !state.Empty() {
		t.Fatalf("state after apply: %s\nwant empty state", spew.Sdump(state))
	}
}

// If a data source explicitly depends on another resource, it's because we need
// that resource to be applied first.
func TestContext2Apply_dataDependsOn(t *testing.T) {
	p := testProvider("null")
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "null_instance" "write" {
  foo = "attribute"
}

data "null_data_source" "read" {
  count = 1
  depends_on = ["null_instance.write"]
}

resource "null_instance" "depends" {
  foo = data.null_data_source.read[0].foo
}
`})

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
	})

	// the "provisioner" here writes to this variable, because the intent is to
	// create a dependency which can't be viewed through the graph, and depends
	// solely on the configuration providing "depends_on"
	provisionerOutput := ""

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		// the side effect of the resource being applied
		provisionerOutput = "APPLIED"
		return testApplyFn(info, s, d)
	}

	p.DiffFn = testDiffFn
	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
		return providers.ReadDataSourceResponse{
			State: cty.ObjectVal(map[string]cty.Value{
				"id":  cty.StringVal("boop"),
				"foo": cty.StringVal(provisionerOutput),
			}),
		}
	}

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	state, diags := ctx.Apply()
	assertNoErrors(t, diags)

	root := state.Module(addrs.RootModuleInstance)
	is := root.ResourceInstance(addrs.Resource{
		Mode: addrs.DataResourceMode,
		Type: "null_data_source",
		Name: "read",
	}.Instance(addrs.IntKey(0)))
	if is == nil {
		t.Fatal("data resource instance is not present in state; should be")
	}
	var attrs map[string]interface{}
	err := json.Unmarshal(is.Current.AttrsJSON, &attrs)
	if err != nil {
		t.Fatal(err)
	}
	actual := attrs["foo"]
	expected := "APPLIED"
	if actual != expected {
		t.Fatalf("bad:\n%s", strings.TrimSpace(state.String()))
	}

	// run another plan to make sure the data source doesn't show as a change
	plan, diags := ctx.Plan()
	assertNoErrors(t, diags)

	for _, c := range plan.Changes.Resources {
		if c.Action != plans.NoOp {
			t.Fatalf("unexpected change for %s", c.Addr)
		}
	}

	// now we cause a change in the first resource, which should trigger a plan
	// in the data source, and the resource that depends on the data source
	// must plan a change as well.
	m = testModuleInline(t, map[string]string{
		"main.tf": `
resource "null_instance" "write" {
  foo = "new"
}

data "null_data_source" "read" {
  depends_on = ["null_instance.write"]
}

resource "null_instance" "depends" {
  foo = data.null_data_source.read.foo
}
`})

	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		// the side effect of the resource being applied
		provisionerOutput = "APPLIED_AGAIN"
		return testApplyFn(info, s, d)
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		State:  state,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
		},
	})

	plan, diags = ctx.Plan()
	assertNoErrors(t, diags)

	expectedChanges := map[string]plans.Action{
		"null_instance.write":        plans.Update,
		"data.null_data_source.read": plans.Read,
		"null_instance.depends":      plans.Update,
	}

	for _, c := range plan.Changes.Resources {
		if c.Action != expectedChanges[c.Addr.String()] {
			t.Errorf("unexpected %s for %s", c.Action, c.Addr)
		}
	}
}

func TestContext2Apply_terraformWorkspace(t *testing.T) {
	m := testModule(t, "apply-terraform-workspace")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Meta:   &ContextMeta{Env: "foo"},
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	actual := state.RootModule().OutputValues["output"]
	expected := cty.StringVal("foo")
	if actual == nil || actual.Value != expected {
		t.Fatalf("wrong value\ngot:  %#v\nwant: %#v", actual.Value, expected)
	}
}

// verify that multiple config references only create a single depends_on entry
func TestContext2Apply_multiRef(t *testing.T) {
	m := testModule(t, "apply-multi-ref")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	deps := state.Modules[""].Resources["aws_instance.other"].Instances[addrs.NoKey].Current.Dependencies
	if len(deps) != 1 || deps[0].String() != "aws_instance.create" {
		t.Fatalf("expected 1 depends_on entry for aws_instance.create, got %q", deps)
	}
}

func TestContext2Apply_targetedModuleRecursive(t *testing.T) {
	m := testModule(t, "apply-targeted-module-recursive")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		Targets: []addrs.Targetable{
			addrs.RootModuleInstance.Child("child", addrs.NoKey),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	mod := state.Module(
		addrs.RootModuleInstance.Child("child", addrs.NoKey).Child("subchild", addrs.NoKey),
	)
	if mod == nil {
		t.Fatalf("no subchild module found in the state!\n\n%#v", state)
	}
	if len(mod.Resources) != 1 {
		t.Fatalf("expected 1 resources, got: %#v", mod.Resources)
	}

	checkStateString(t, state, `
<no state>
module.child.subchild:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    num = 2
    type = aws_instance
	`)
}

func TestContext2Apply_localVal(t *testing.T) {
	m := testModule(t, "apply-local-val")
	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: map[addrs.Provider]providers.Factory{},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("error during plan: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("error during apply: %s", diags.Err())
	}

	got := strings.TrimSpace(state.String())
	want := strings.TrimSpace(`
<no state>
Outputs:

result_1 = hello
result_3 = hello world
`)
	if got != want {
		t.Fatalf("wrong final state\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestContext2Apply_destroyWithLocals(t *testing.T) {
	m := testModule(t, "apply-destroy-with-locals")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = func(info *InstanceInfo, s *InstanceState, c *ResourceConfig) (*InstanceDiff, error) {
		d, err := testDiffFn(info, s, c)
		return d, err
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.foo").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetOutputValue("name", cty.StringVal("test-bar"), false)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	s, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("error during apply: %s", diags.Err())
	}

	got := strings.TrimSpace(s.String())
	want := strings.TrimSpace(`<no state>`)
	if got != want {
		t.Fatalf("wrong final state\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestContext2Apply_providerWithLocals(t *testing.T) {
	m := testModule(t, "provider-with-locals")
	p := testProvider("aws")

	providerRegion := ""
	// this should not be overridden during destroy
	p.ConfigureFn = func(c *ResourceConfig) error {
		if r, ok := c.Get("region"); ok {
			providerRegion = r.(string)
		}
		return nil
	}
	p.DiffFn = testDiffFn
	p.ApplyFn = testApplyFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	if state.HasResources() {
		t.Fatal("expected no state, got:", state)
	}

	if providerRegion != "bar" {
		t.Fatalf("expected region %q, got: %q", "bar", providerRegion)
	}
}

func TestContext2Apply_destroyWithProviders(t *testing.T) {
	m := testModule(t, "destroy-module-with-provider")
	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	removed := state.EnsureModule(addrs.RootModuleInstance.Child("mod", addrs.NoKey).Child("removed", addrs.NoKey))
	removed.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.child").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"].baz`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	// test that we can't destroy if the provider is missing
	if _, diags := ctx.Plan(); diags == nil {
		t.Fatal("expected plan error, provider.aws.baz doesn't exist")
	}

	// correct the state
	state.Modules["module.mod.module.removed"].Resources["aws_instance.child"].ProviderConfig = mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"].bar`)

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatal(diags.Err())
	}
	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("error during apply: %s", diags.Err())
	}

	got := strings.TrimSpace(state.String())

	want := strings.TrimSpace("<no state>")
	if got != want {
		t.Fatalf("wrong final state\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestContext2Apply_providersFromState(t *testing.T) {
	m := configs.NewEmptyConfig()
	p := testProvider("aws")
	p.DiffFn = testDiffFn

	implicitProviderState := states.NewState()
	impRoot := implicitProviderState.EnsureModule(addrs.RootModuleInstance)
	impRoot.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	aliasedProviderState := states.NewState()
	aliasRoot := aliasedProviderState.EnsureModule(addrs.RootModuleInstance)
	aliasRoot.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"].bar`),
	)

	moduleProviderState := states.NewState()
	moduleProviderRoot := moduleProviderState.EnsureModule(addrs.RootModuleInstance)
	moduleProviderRoot.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"bar"}`),
		},
		mustProviderConfig(`module.child.provider["registry.terraform.io/hashicorp/aws"]`),
	)

	for _, tc := range []struct {
		name   string
		state  *states.State
		output string
		err    bool
	}{
		{
			name:   "add implicit provider",
			state:  implicitProviderState,
			err:    false,
			output: "<no state>",
		},

		// an aliased provider must be in the config to remove a resource
		{
			name:  "add aliased provider",
			state: aliasedProviderState,
			err:   true,
		},

		// a provider in a module implies some sort of config, so this isn't
		// allowed even without an alias
		{
			name:  "add unaliased module provider",
			state: moduleProviderState,
			err:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext2(t, &ContextOpts{
				Config: m,
				Providers: map[addrs.Provider]providers.Factory{
					addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
				},
				State: tc.state,
			})

			_, diags := ctx.Plan()
			if tc.err {
				if diags == nil {
					t.Fatal("expected error")
				} else {
					return
				}
			}
			if !tc.err && diags.HasErrors() {
				t.Fatal(diags.Err())
			}

			state, diags := ctx.Apply()
			if diags.HasErrors() {
				t.Fatalf("diags: %s", diags.Err())
			}

			checkStateString(t, state, "<no state>")

		})
	}
}

func TestContext2Apply_plannedInterpolatedCount(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-interpolated-count")

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.test").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: Providers,
		State:     state,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("plan failed: %s", diags.Err())
	}

	// We'll marshal and unmarshal the plan here, to ensure that we have
	// a clean new context as would be created if we separately ran
	// terraform plan -out=tfplan && terraform apply tfplan
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatalf("failed to round-trip through planfile: %s", err)
	}

	ctxOpts.Providers = Providers
	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	// Applying the plan should now succeed
	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply failed: %s", diags.Err())
	}
}

func TestContext2Apply_plannedDestroyInterpolatedCount(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "plan-destroy-interpolated-count")

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.a[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetOutputValue("out", cty.ListVal([]cty.Value{cty.StringVal("foo"), cty.StringVal("foo")}), false)

	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: providers,
		State:     state,
		Destroy:   true,
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("plan failed: %s", diags.Err())
	}

	// We'll marshal and unmarshal the plan here, to ensure that we have
	// a clean new context as would be created if we separately ran
	// terraform plan -out=tfplan && terraform apply tfplan
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatalf("failed to round-trip through planfile: %s", err)
	}

	ctxOpts.Providers = providers
	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("err: %s", diags.Err())
	}

	// Applying the plan should now succeed
	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply failed: %s", diags.Err())
	}
}

func TestContext2Apply_scaleInMultivarRef(t *testing.T) {
	m := testModule(t, "apply-resource-scale-in")

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.one").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("aws_instance.two").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"foo"}`),
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/aws"]`),
	)

	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: Providers,
		State:     state,
		Variables: InputValues{
			"instance_count": {
				Value:      cty.NumberIntVal(0),
				SourceType: ValueFromCaller,
			},
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	// Applying the plan should now succeed
	_, diags = ctx.Apply()
	assertNoErrors(t, diags)
}

func TestContext2Apply_inconsistentWithPlan(t *testing.T) {
	m := testModule(t, "apply-inconsistent-with-plan")
	p := testProvider("test")
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test": {
				Attributes: map[string]*configschema.Attribute{
					"id": {Type: cty.String, Computed: true},
				},
			},
		},
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: cty.ObjectVal(map[string]cty.Value{
				"id": cty.StringVal("before"),
			}),
		}
	}
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		return providers.ApplyResourceChangeResponse{
			NewState: cty.ObjectVal(map[string]cty.Value{
				// This is intentionally incorrect: because id was fixed at "before"
				// during plan, it must not change during apply.
				"id": cty.StringVal("after"),
			}),
		}
	}
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags := ctx.Apply()
	if !diags.HasErrors() {
		t.Fatalf("apply succeeded; want error")
	}
	if got, want := diags.Err().Error(), "Provider produced inconsistent result after apply"; !strings.Contains(got, want) {
		t.Fatalf("wrong error\ngot: %s\nshould contain: %s", got, want)
	}
}

// Issue 19908 was about retaining an existing object in the state when an
// update to it fails and the provider does not return a partially-updated
// value for it. Previously we were incorrectly removing it from the state
// in that case, but instead it should be retained so the update can be
// retried.
func TestContext2Apply_issue19908(t *testing.T) {
	m := testModule(t, "apply-issue19908")
	p := testProvider("test")
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test": {
				Attributes: map[string]*configschema.Attribute{
					"baz": {Type: cty.String, Required: true},
				},
			},
		},
	}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		var diags tfdiags.Diagnostics
		diags = diags.Append(fmt.Errorf("update failed"))
		return providers.ApplyResourceChangeResponse{
			Diagnostics: diags,
		}
	}
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		State: states.BuildState(func(s *states.SyncState) {
			s.SetResourceInstanceCurrent(
				addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test",
					Name: "foo",
				}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
				&states.ResourceInstanceObjectSrc{
					AttrsJSON: []byte(`{"baz":"old"}`),
					Status:    states.ObjectReady,
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				},
			)
		}),
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if !diags.HasErrors() {
		t.Fatalf("apply succeeded; want error")
	}
	if got, want := diags.Err().Error(), "update failed"; !strings.Contains(got, want) {
		t.Fatalf("wrong error\ngot: %s\nshould contain: %s", got, want)
	}

	mod := state.RootModule()
	rs := mod.Resources["test.foo"]
	if rs == nil {
		t.Fatalf("test.foo not in state after apply, but should be")
	}
	is := rs.Instances[addrs.NoKey]
	if is == nil {
		t.Fatalf("test.foo not in state after apply, but should be")
	}
	obj := is.Current
	if obj == nil {
		t.Fatalf("test.foo has no current object in state after apply, but should do")
	}

	if got, want := obj.Status, states.ObjectReady; got != want {
		t.Errorf("test.foo has wrong status %s after apply; want %s", got, want)
	}
	if got, want := obj.AttrsJSON, []byte(`"old"`); !bytes.Contains(got, want) {
		t.Errorf("test.foo attributes JSON doesn't contain %s after apply\ngot: %s", want, got)
	}
}

func TestContext2Apply_invalidIndexRef(t *testing.T) {
	p := testProvider("test")
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_instance": {
				Attributes: map[string]*configschema.Attribute{
					"value": {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
	}
	p.DiffFn = testDiffFn

	m := testModule(t, "apply-invalid-index")
	c := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	diags := c.Validate()
	if diags.HasErrors() {
		t.Fatalf("unexpected validation failure: %s", diags.Err())
	}

	wantErr := `The given key does not identify an element in this collection value`
	_, diags = c.Plan()

	if !diags.HasErrors() {
		t.Fatalf("plan succeeded; want error")
	}
	gotErr := diags.Err().Error()

	if !strings.Contains(gotErr, wantErr) {
		t.Fatalf("missing expected error\ngot: %s\n\nwant: error containing %q", gotErr, wantErr)
	}
}

func TestContext2Apply_moduleReplaceCycle(t *testing.T) {
	for _, mode := range []string{"normal", "cbd"} {
		var m *configs.Config

		switch mode {
		case "normal":
			m = testModule(t, "apply-module-replace-cycle")
		case "cbd":
			m = testModule(t, "apply-module-replace-cycle-cbd")
		}

		p := testProvider("aws")
		p.DiffFn = testDiffFn
		p.ApplyFn = testApplyFn

		instanceSchema := &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"id":          {Type: cty.String, Computed: true},
				"require_new": {Type: cty.String, Optional: true},
			},
		}

		p.GetSchemaReturn = &ProviderSchema{
			ResourceTypes: map[string]*configschema.Block{
				"aws_instance": instanceSchema,
			},
		}

		state := states.NewState()
		modA := state.EnsureModule(addrs.RootModuleInstance.Child("a", addrs.NoKey))
		modA.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "aws_instance",
				Name: "a",
			}.Instance(addrs.NoKey),
			&states.ResourceInstanceObjectSrc{
				Status:              states.ObjectReady,
				AttrsJSON:           []byte(`{"id":"a","require_new":"old"}`),
				CreateBeforeDestroy: mode == "cbd",
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("aws"),
				Module:   addrs.RootModule,
			},
		)

		modB := state.EnsureModule(addrs.RootModuleInstance.Child("b", addrs.NoKey))
		modB.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "aws_instance",
				Name: "b",
			}.Instance(addrs.IntKey(0)),
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(`{"id":"b","require_new":"old"}`),
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("aws"),
				Module:   addrs.RootModule,
			},
		)

		aBefore, _ := plans.NewDynamicValue(
			cty.ObjectVal(map[string]cty.Value{
				"id":          cty.StringVal("a"),
				"require_new": cty.StringVal("old"),
			}), instanceSchema.ImpliedType())
		aAfter, _ := plans.NewDynamicValue(
			cty.ObjectVal(map[string]cty.Value{
				"id":          cty.UnknownVal(cty.String),
				"require_new": cty.StringVal("new"),
			}), instanceSchema.ImpliedType())
		bBefore, _ := plans.NewDynamicValue(
			cty.ObjectVal(map[string]cty.Value{
				"id":          cty.StringVal("b"),
				"require_new": cty.StringVal("old"),
			}), instanceSchema.ImpliedType())
		bAfter, _ := plans.NewDynamicValue(
			cty.ObjectVal(map[string]cty.Value{
				"id":          cty.UnknownVal(cty.String),
				"require_new": cty.UnknownVal(cty.String),
			}), instanceSchema.ImpliedType())

		var aAction plans.Action
		switch mode {
		case "normal":
			aAction = plans.DeleteThenCreate
		case "cbd":
			aAction = plans.CreateThenDelete
		}

		changes := &plans.Changes{
			Resources: []*plans.ResourceInstanceChangeSrc{
				{
					Addr: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "a",
					}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance.Child("a", addrs.NoKey)),
					ProviderAddr: addrs.AbsProviderConfig{
						Provider: addrs.NewDefaultProvider("aws"),
						Module:   addrs.RootModule,
					},
					ChangeSrc: plans.ChangeSrc{
						Action: aAction,
						Before: aBefore,
						After:  aAfter,
					},
				},
				{
					Addr: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "aws_instance",
						Name: "b",
					}.Instance(addrs.IntKey(0)).Absolute(addrs.RootModuleInstance.Child("b", addrs.NoKey)),
					ProviderAddr: addrs.AbsProviderConfig{
						Provider: addrs.NewDefaultProvider("aws"),
						Module:   addrs.RootModule,
					},
					ChangeSrc: plans.ChangeSrc{
						Action: plans.DeleteThenCreate,
						Before: bBefore,
						After:  bAfter,
					},
				},
			},
		}

		ctx := testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
			},
			State:   state,
			Changes: changes,
		})

		t.Run(mode, func(t *testing.T) {
			_, diags := ctx.Apply()
			if diags.HasErrors() {
				t.Fatal(diags.Err())
			}
		})
	}
}

func TestContext2Apply_destroyDataCycle(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-destroy-data-cycle")
	p := testProvider("null")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "null_resource",
			Name: "a",
		}.Instance(addrs.IntKey(0)),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"a"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("null"),
			Module:   addrs.RootModule,
		},
	)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.DataResourceMode,
			Type: "null_data_source",
			Name: "d",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"data"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("null"),
			Module:   addrs.RootModule,
		},
	)

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("null"): testProviderFuncFixed(p),
	}

	hook := &testHook{}
	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: Providers,
		State:     state,
		Destroy:   true,
		Hooks:     []Hook{hook},
	})

	plan, diags := ctx.Plan()
	diags.HasErrors()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// We'll marshal and unmarshal the plan here, to ensure that we have
	// a clean new context as would be created if we separately ran
	// terraform plan -out=tfplan && terraform apply tfplan
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatal(err)
	}
	ctxOpts.Providers = Providers
	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("failed to create context for plan: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}
}

func TestContext2Apply_taintedDestroyFailure(t *testing.T) {
	m := testModule(t, "apply-destroy-tainted")
	p := testProvider("test")
	p.DiffFn = testDiffFn
	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		// All destroys fail.
		// c will also fail to create, meaning the existing tainted instance
		// becomes deposed, ans is then promoted back to current.
		// only C has a foo attribute
		attr := d.Attributes["foo"]
		if d.Destroy || (attr != nil && attr.New == "c") {
			return nil, errors.New("failure")
		}

		return testApplyFn(info, s, d)
	}
	p.GetSchemaReturn = &ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"test_instance": {
				Attributes: map[string]*configschema.Attribute{
					"id": {
						Type:     cty.String,
						Computed: true,
					},
					"foo": {
						Type:     cty.String,
						Optional: true,
					},
				},
			},
		},
	}

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "a",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"a","foo":"a"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
	)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "b",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"b","foo":"b"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
	)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "c",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectTainted,
			AttrsJSON: []byte(`{"id":"c","foo":"old"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
	)

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
	}

	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: Providers,
		State:     state,
		Hooks:     []Hook{&testHook{}},
	})

	_, diags := ctx.Plan()
	diags.HasErrors()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if !diags.HasErrors() {
		t.Fatal("expected error")
	}

	root = state.Module(addrs.RootModuleInstance)

	// the instance that failed to destroy should remain tainted
	a := root.ResourceInstance(addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_instance",
		Name: "a",
	}.Instance(addrs.NoKey))

	if a.Current.Status != states.ObjectTainted {
		t.Fatal("test_instance.a should be tainted")
	}

	// b is create_before_destroy, and the destroy failed, so there should be 1
	// deposed instance.
	b := root.ResourceInstance(addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_instance",
		Name: "b",
	}.Instance(addrs.NoKey))

	if b.Current.Status != states.ObjectReady {
		t.Fatal("test_instance.b should be Ready")
	}

	if len(b.Deposed) != 1 {
		t.Fatal("test_instance.b failed to keep deposed instance")
	}

	// the desposed c instance should be promoted back to Current, and remain
	// tainted
	c := root.ResourceInstance(addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_instance",
		Name: "c",
	}.Instance(addrs.NoKey))

	if c.Current == nil {
		t.Fatal("test_instance.c has no current instance, but it should")
	}

	if c.Current.Status != states.ObjectTainted {
		t.Fatal("test_instance.c should be tainted")
	}

	if len(c.Deposed) != 0 {
		t.Fatal("test_instance.c should have no deposed instances")
	}

	if string(c.Current.AttrsJSON) != `{"foo":"old","id":"c"}` {
		t.Fatalf("unexpected attrs for c: %q\n", c.Current.AttrsJSON)
	}
}

func TestContext2Apply_plannedConnectionRefs(t *testing.T) {
	m := testModule(t, "apply-plan-connection-refs")
	p := testProvider("test")
	p.DiffFn = testDiffFn
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
		s := req.PlannedState.AsValueMap()
		// delay "a" slightly, so if the reference edge is missing the "b"
		// provisioner will see an unknown value.
		if s["foo"].AsString() == "a" {
			time.Sleep(500 * time.Millisecond)
		}

		s["id"] = cty.StringVal("ID")
		resp.NewState = cty.ObjectVal(s)
		return resp
	}

	pr := testProvisioner()
	pr.ProvisionResourceFn = func(req provisioners.ProvisionResourceRequest) (resp provisioners.ProvisionResourceResponse) {
		host := req.Connection.GetAttr("host")
		if host.IsNull() || !host.IsKnown() {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("invalid host value: %#v", host))
		}

		return resp
	}

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
	}

	provisioners := map[string]ProvisionerFactory{
		"shell": testProvisionerFuncFixed(pr),
	}

	hook := &testHook{}
	ctx := testContext2(t, &ContextOpts{
		Config:       m,
		Providers:    Providers,
		Provisioners: provisioners,
		Hooks:        []Hook{hook},
	})

	_, diags := ctx.Plan()
	diags.HasErrors()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}
}

func TestContext2Apply_cbdCycle(t *testing.T) {
	m, snap := testModuleWithSnapshot(t, "apply-cbd-cycle")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "a",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"a","require_new":"old","foo":"b"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "b",
					},
					Module: addrs.RootModule,
				},
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "c",
					},
					Module: addrs.RootModule,
				},
			},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
	)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "b",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"b","require_new":"old","foo":"c"}`),
			Dependencies: []addrs.ConfigResource{
				addrs.ConfigResource{
					Resource: addrs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "c",
					},
					Module: addrs.RootModule,
				},
			},
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
	)
	root.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_instance",
			Name: "c",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"c","require_new":"old"}`),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("test"),
			Module:   addrs.RootModule,
		},
	)

	Providers := map[addrs.Provider]providers.Factory{
		addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
	}

	hook := &testHook{}
	ctx := testContext2(t, &ContextOpts{
		Config:    m,
		Providers: Providers,
		State:     state,
		Hooks:     []Hook{hook},
	})

	plan, diags := ctx.Plan()
	diags.HasErrors()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}

	// We'll marshal and unmarshal the plan here, to ensure that we have
	// a clean new context as would be created if we separately ran
	// terraform plan -out=tfplan && terraform apply tfplan
	ctxOpts, err := contextOptsForPlanViaFile(snap, plan)
	if err != nil {
		t.Fatal(err)
	}
	ctxOpts.Providers = Providers
	ctx, diags = NewContext(ctxOpts)
	if diags.HasErrors() {
		t.Fatalf("failed to create context for plan: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("diags: %s", diags.Err())
	}
}

func TestContext2Apply_ProviderMeta_apply_set(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}

	var pmMu sync.Mutex
	arcPMs := map[string]cty.Value{}

	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		pmMu.Lock()
		defer pmMu.Unlock()
		arcPMs[req.TypeName] = req.ProviderMeta

		s := req.PlannedState.AsValueMap()
		s["id"] = cty.StringVal("ID")
		return providers.ApplyResourceChangeResponse{
			NewState: cty.ObjectVal(s),
		}
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	if !p.ApplyResourceChangeCalled {
		t.Fatalf("ApplyResourceChange not called")
	}

	expectations := map[string]cty.Value{}

	if pm, ok := arcPMs["test_resource"]; !ok {
		t.Fatalf("sub-module ApplyResourceChange not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in sub-module ApplyResourceChange")
	} else {
		expectations["quux-submodule"] = pm
	}

	if pm, ok := arcPMs["test_instance"]; !ok {
		t.Fatalf("root module ApplyResourceChange not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in root module ApplyResourceChange")
	} else {
		expectations["quux"] = pm
	}

	type metaStruct struct {
		Baz string `cty:"baz"`
	}

	for expected, v := range expectations {
		var meta metaStruct
		err := gocty.FromCtyValue(v, &meta)
		if err != nil {
			t.Fatalf("Error parsing cty value: %s", err)
		}
		if meta.Baz != expected {
			t.Fatalf("Expected meta.Baz to be %q, got %q", expected, meta.Baz)
		}
	}
}

func TestContext2Apply_ProviderMeta_apply_unset(t *testing.T) {
	m := testModule(t, "provider-meta-unset")
	p := testProvider("test")
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	var pmMu sync.Mutex
	arcPMs := map[string]cty.Value{}
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		pmMu.Lock()
		defer pmMu.Unlock()
		arcPMs[req.TypeName] = req.ProviderMeta

		s := req.PlannedState.AsValueMap()
		s["id"] = cty.StringVal("ID")
		return providers.ApplyResourceChangeResponse{
			NewState: cty.ObjectVal(s),
		}
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	if !p.ApplyResourceChangeCalled {
		t.Fatalf("ApplyResourceChange not called")
	}

	if pm, ok := arcPMs["test_resource"]; !ok {
		t.Fatalf("sub-module ApplyResourceChange not called")
	} else if !pm.IsNull() {
		t.Fatalf("non-null ProviderMeta in sub-module ApplyResourceChange: %+v", pm)
	}

	if pm, ok := arcPMs["test_instance"]; !ok {
		t.Fatalf("root module ApplyResourceChange not called")
	} else if !pm.IsNull() {
		t.Fatalf("non-null ProviderMeta in root module ApplyResourceChange: %+v", pm)
	}
}

func TestContext2Apply_ProviderMeta_plan_set(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	prcPMs := map[string]cty.Value{}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		prcPMs[req.TypeName] = req.ProviderMeta
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	if !p.PlanResourceChangeCalled {
		t.Fatalf("PlanResourceChange not called")
	}

	expectations := map[string]cty.Value{}

	if pm, ok := prcPMs["test_resource"]; !ok {
		t.Fatalf("sub-module PlanResourceChange not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in sub-module PlanResourceChange")
	} else {
		expectations["quux-submodule"] = pm
	}

	if pm, ok := prcPMs["test_instance"]; !ok {
		t.Fatalf("root module PlanResourceChange not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in root module PlanResourceChange")
	} else {
		expectations["quux"] = pm
	}

	type metaStruct struct {
		Baz string `cty:"baz"`
	}

	for expected, v := range expectations {
		var meta metaStruct
		err := gocty.FromCtyValue(v, &meta)
		if err != nil {
			t.Fatalf("Error parsing cty value: %s", err)
		}
		if meta.Baz != expected {
			t.Fatalf("Expected meta.Baz to be %q, got %q", expected, meta.Baz)
		}
	}
}

func TestContext2Apply_ProviderMeta_plan_unset(t *testing.T) {
	m := testModule(t, "provider-meta-unset")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	prcPMs := map[string]cty.Value{}
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		prcPMs[req.TypeName] = req.ProviderMeta
		return providers.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState,
		}
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	if !p.PlanResourceChangeCalled {
		t.Fatalf("PlanResourceChange not called")
	}

	if pm, ok := prcPMs["test_resource"]; !ok {
		t.Fatalf("sub-module PlanResourceChange not called")
	} else if !pm.IsNull() {
		t.Fatalf("non-null ProviderMeta in sub-module PlanResourceChange: %+v", pm)
	}

	if pm, ok := prcPMs["test_instance"]; !ok {
		t.Fatalf("root module PlanResourceChange not called")
	} else if !pm.IsNull() {
		t.Fatalf("non-null ProviderMeta in root module PlanResourceChange: %+v", pm)
	}
}

func TestContext2Apply_ProviderMeta_plan_setNoSchema(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if !diags.HasErrors() {
		t.Fatalf("plan supposed to error, has no errors")
	}

	var rootErr, subErr bool
	errorSummary := "The resource test_%s.bar belongs to a provider that doesn't support provider_meta blocks"
	for _, diag := range diags {
		if diag.Description().Summary != "Provider registry.terraform.io/hashicorp/test doesn't support provider_meta" {
			t.Errorf("Unexpected error: %+v", diag.Description())
		}
		switch diag.Description().Detail {
		case fmt.Sprintf(errorSummary, "instance"):
			rootErr = true
		case fmt.Sprintf(errorSummary, "resource"):
			subErr = true
		default:
			t.Errorf("Unexpected error: %s", diag.Description())
		}
	}
	if !rootErr {
		t.Errorf("Expected unsupported provider_meta block error for root module, none received")
	}
	if !subErr {
		t.Errorf("Expected unsupported provider_meta block error for sub-module, none received")
	}
}

func TestContext2Apply_ProviderMeta_plan_setInvalid(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"quux": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if !diags.HasErrors() {
		t.Fatalf("plan supposed to error, has no errors")
	}

	var reqErr, invalidErr bool
	for _, diag := range diags {
		switch diag.Description().Summary {
		case "Missing required argument":
			if diag.Description().Detail == `The argument "quux" is required, but no definition was found.` {
				reqErr = true
			} else {
				t.Errorf("Unexpected error %+v", diag.Description())
			}
		case "Unsupported argument":
			if diag.Description().Detail == `An argument named "baz" is not expected here.` {
				invalidErr = true
			} else {
				t.Errorf("Unexpected error %+v", diag.Description())
			}
		default:
			t.Errorf("Unexpected error %+v", diag.Description())
		}
	}
	if !reqErr {
		t.Errorf("Expected missing required argument error, none received")
	}
	if !invalidErr {
		t.Errorf("Expected unsupported argument error, none received")
	}
}

func TestContext2Apply_ProviderMeta_refresh_set(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	rrcPMs := map[string]cty.Value{}
	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		rrcPMs[req.TypeName] = req.ProviderMeta
		newState, err := p.GetSchemaReturn.ResourceTypes[req.TypeName].CoerceValue(p.ReadResourceResponse.NewState)
		if err != nil {
			panic(err)
		}
		resp := p.ReadResourceResponse
		resp.NewState = newState
		return resp
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	_, diags = ctx.Refresh()
	assertNoErrors(t, diags)

	if !p.ReadResourceCalled {
		t.Fatalf("ReadResource not called")
	}

	expectations := map[string]cty.Value{}

	if pm, ok := rrcPMs["test_resource"]; !ok {
		t.Fatalf("sub-module ReadResource not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in sub-module ReadResource")
	} else {
		expectations["quux-submodule"] = pm
	}

	if pm, ok := rrcPMs["test_instance"]; !ok {
		t.Fatalf("root module ReadResource not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in root module ReadResource")
	} else {
		expectations["quux"] = pm
	}

	type metaStruct struct {
		Baz string `cty:"baz"`
	}

	for expected, v := range expectations {
		var meta metaStruct
		err := gocty.FromCtyValue(v, &meta)
		if err != nil {
			t.Fatalf("Error parsing cty value: %s", err)
		}
		if meta.Baz != expected {
			t.Fatalf("Expected meta.Baz to be %q, got %q", expected, meta.Baz)
		}
	}
}

func TestContext2Apply_ProviderMeta_refresh_setNoSchema(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	// we need a schema for plan/apply so they don't error
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	// drop the schema before refresh, to test that it errors
	schema.ProviderMeta = nil
	p.GetSchemaReturn = schema
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: ctx.State(),
	})

	_, diags = ctx.Refresh()
	if !diags.HasErrors() {
		t.Fatalf("refresh supposed to error, has no errors")
	}

	var rootErr, subErr bool
	errorSummary := "The resource test_%s.bar belongs to a provider that doesn't support provider_meta blocks"
	for _, diag := range diags {
		if diag.Description().Summary != "Provider registry.terraform.io/hashicorp/test doesn't support provider_meta" {
			t.Errorf("Unexpected error: %+v", diag.Description())
		}
		switch diag.Description().Detail {
		case fmt.Sprintf(errorSummary, "instance"):
			rootErr = true
		case fmt.Sprintf(errorSummary, "resource"):
			subErr = true
		default:
			t.Errorf("Unexpected error: %s", diag.Description())
		}
	}
	if !rootErr {
		t.Errorf("Expected unsupported provider_meta block error for root module, none received")
	}
	if !subErr {
		t.Errorf("Expected unsupported provider_meta block error for sub-module, none received")
	}
}

func TestContext2Apply_ProviderMeta_refresh_setInvalid(t *testing.T) {
	m := testModule(t, "provider-meta-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	// we need a matching schema for plan/apply so they don't error
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	// change the schema before refresh, to test that it errors
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"quux": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: ctx.State(),
	})

	_, diags = ctx.Refresh()
	if !diags.HasErrors() {
		t.Fatalf("refresh supposed to error, has no errors")
	}

	var reqErr, invalidErr bool
	for _, diag := range diags {
		switch diag.Description().Summary {
		case "Missing required argument":
			if diag.Description().Detail == `The argument "quux" is required, but no definition was found.` {
				reqErr = true
			} else {
				t.Errorf("Unexpected error %+v", diag.Description())
			}
		case "Unsupported argument":
			if diag.Description().Detail == `An argument named "baz" is not expected here.` {
				invalidErr = true
			} else {
				t.Errorf("Unexpected error %+v", diag.Description())
			}
		default:
			t.Errorf("Unexpected error %+v", diag.Description())
		}
	}
	if !reqErr {
		t.Errorf("Expected missing required argument error, none received")
	}
	if !invalidErr {
		t.Errorf("Expected unsupported argument error, none received")
	}
}

func TestContext2Apply_ProviderMeta_refreshdata_set(t *testing.T) {
	m := testModule(t, "provider-meta-data-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	rdsPMs := map[string]cty.Value{}
	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
		rdsPMs[req.TypeName] = req.ProviderMeta
		switch req.TypeName {
		case "test_data_source":
			log.Printf("[TRACE] test_data_source RDSR returning")
			return providers.ReadDataSourceResponse{
				State: cty.ObjectVal(map[string]cty.Value{
					"id":  cty.StringVal("yo"),
					"foo": cty.StringVal("bar"),
				}),
			}
		case "test_file":
			log.Printf("[TRACE] test_file RDSR returning")
			return providers.ReadDataSourceResponse{
				State: cty.ObjectVal(map[string]cty.Value{
					"id":       cty.StringVal("bar"),
					"rendered": cty.StringVal("baz"),
					"template": cty.StringVal(""),
				}),
			}
		default:
			// config drift, oops
			log.Printf("[TRACE] unknown request TypeName: %q", req.TypeName)
			return providers.ReadDataSourceResponse{}
		}
	}

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	_, diags = ctx.Refresh()
	assertNoErrors(t, diags)

	if !p.ReadDataSourceCalled {
		t.Fatalf("ReadDataSource not called")
	}

	expectations := map[string]cty.Value{}

	if pm, ok := rdsPMs["test_file"]; !ok {
		t.Fatalf("sub-module ReadDataSource not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in sub-module ReadDataSource")
	} else {
		expectations["quux-submodule"] = pm
	}

	if pm, ok := rdsPMs["test_data_source"]; !ok {
		t.Fatalf("root module ReadDataSource not called")
	} else if pm.IsNull() {
		t.Fatalf("null ProviderMeta in root module ReadDataSource")
	} else {
		expectations["quux"] = pm
	}

	type metaStruct struct {
		Baz string `cty:"baz"`
	}

	for expected, v := range expectations {
		var meta metaStruct
		err := gocty.FromCtyValue(v, &meta)
		if err != nil {
			t.Fatalf("Error parsing cty value: %s", err)
		}
		if meta.Baz != expected {
			t.Fatalf("Expected meta.Baz to be %q, got %q", expected, meta.Baz)
		}
	}
}

func TestContext2Apply_ProviderMeta_refreshdata_unset(t *testing.T) {
	m := testModule(t, "provider-meta-data-unset")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"baz": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	rdsPMs := map[string]cty.Value{}
	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
		rdsPMs[req.TypeName] = req.ProviderMeta
		switch req.TypeName {
		case "test_data_source":
			return providers.ReadDataSourceResponse{
				State: cty.ObjectVal(map[string]cty.Value{
					"id":  cty.StringVal("yo"),
					"foo": cty.StringVal("bar"),
				}),
			}
		case "test_file":
			return providers.ReadDataSourceResponse{
				State: cty.ObjectVal(map[string]cty.Value{
					"id":       cty.StringVal("bar"),
					"rendered": cty.StringVal("baz"),
					"template": cty.StringVal(""),
				}),
			}
		default:
			// config drift, oops
			return providers.ReadDataSourceResponse{}
		}
	}

	_, diags := ctx.Plan()
	assertNoErrors(t, diags)

	_, diags = ctx.Apply()
	assertNoErrors(t, diags)

	if !p.ReadDataSourceCalled {
		t.Fatalf("ReadDataSource not called")
	}

	if pm, ok := rdsPMs["test_file"]; !ok {
		t.Fatalf("sub-module ReadDataSource not called")
	} else if !pm.IsNull() {
		t.Fatalf("non-null ProviderMeta in sub-module ReadDataSource")
	}

	if pm, ok := rdsPMs["test_data_source"]; !ok {
		t.Fatalf("root module ReadDataSource not called")
	} else if !pm.IsNull() {
		t.Fatalf("non-null ProviderMeta in root module ReadDataSource")
	}
}

func TestContext2Apply_ProviderMeta_refreshdata_setNoSchema(t *testing.T) {
	m := testModule(t, "provider-meta-data-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	p.ReadDataSourceResponse = providers.ReadDataSourceResponse{
		State: cty.ObjectVal(map[string]cty.Value{
			"id":  cty.StringVal("yo"),
			"foo": cty.StringVal("bar"),
		}),
	}

	_, diags := ctx.Refresh()
	if !diags.HasErrors() {
		t.Fatalf("refresh supposed to error, has no errors")
	}

	var rootErr, subErr bool
	errorSummary := "The resource data.test_%s.foo belongs to a provider that doesn't support provider_meta blocks"
	for _, diag := range diags {
		if diag.Description().Summary != "Provider registry.terraform.io/hashicorp/test doesn't support provider_meta" {
			t.Errorf("Unexpected error: %+v", diag.Description())
		}
		switch diag.Description().Detail {
		case fmt.Sprintf(errorSummary, "data_source"):
			rootErr = true
		case fmt.Sprintf(errorSummary, "file"):
			subErr = true
		default:
			t.Errorf("Unexpected error: %s", diag.Description())
		}
	}
	if !rootErr {
		t.Errorf("Expected unsupported provider_meta block error for root module, none received")
	}
	if !subErr {
		t.Errorf("Expected unsupported provider_meta block error for sub-module, none received")
	}
}

func TestContext2Apply_ProviderMeta_refreshdata_setInvalid(t *testing.T) {
	m := testModule(t, "provider-meta-data-set")
	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	schema := p.GetSchemaReturn
	schema.ProviderMeta = &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"quux": {
				Type:     cty.String,
				Required: true,
			},
		},
	}
	p.GetSchemaReturn = schema
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})
	p.ReadDataSourceResponse = providers.ReadDataSourceResponse{
		State: cty.ObjectVal(map[string]cty.Value{
			"id":  cty.StringVal("yo"),
			"foo": cty.StringVal("bar"),
		}),
	}

	_, diags := ctx.Refresh()
	if !diags.HasErrors() {
		t.Fatalf("refresh supposed to error, has no errors")
	}

	var reqErr, invalidErr bool
	for _, diag := range diags {
		switch diag.Description().Summary {
		case "Missing required argument":
			if diag.Description().Detail == `The argument "quux" is required, but no definition was found.` {
				reqErr = true
			} else {
				t.Errorf("Unexpected error %+v", diag.Description())
			}
		case "Unsupported argument":
			if diag.Description().Detail == `An argument named "baz" is not expected here.` {
				invalidErr = true
			} else {
				t.Errorf("Unexpected error %+v", diag.Description())
			}
		default:
			t.Errorf("Unexpected error %+v", diag.Description())
		}
	}
	if !reqErr {
		t.Errorf("Expected missing required argument error, none received")
	}
	if !invalidErr {
		t.Errorf("Expected unsupported argument error, none received")
	}
}

func TestContext2Apply_expandModuleVariables(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
module "mod1" {
  for_each = toset(["a"])
  source = "./mod"
}

module "mod2" {
  source = "./mod"
  in = module.mod1["a"].out
}
`,
		"mod/main.tf": `
resource "aws_instance" "foo" {
  foo = var.in
}

variable "in" {
  type = string
  default = "default"
}

output "out" {
  value = aws_instance.foo.id
}
`,
	})

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	expected := `<no state>
module.mod1["a"]:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    foo = default
    type = aws_instance

  Outputs:

  out = foo
module.mod2:
  aws_instance.foo:
    ID = foo
    provider = provider["registry.terraform.io/hashicorp/aws"]
    foo = foo
    type = aws_instance

    Dependencies:
      module.mod1.aws_instance.foo`

	if state.String() != expected {
		t.Fatalf("expected:\n%s\ngot:\n%s\n", expected, state)
	}
}

func TestContext2Apply_inheritAndStoreCBD(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
resource "aws_instance" "foo" {
}

resource "aws_instance" "cbd" {
  foo = aws_instance.foo.id
  lifecycle {
    create_before_destroy = true
  }
}
`,
	})

	p := testProvider("aws")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	foo := state.ResourceInstance(mustResourceInstanceAddr("aws_instance.foo"))
	if !foo.Current.CreateBeforeDestroy {
		t.Fatal("aws_instance.foo should also be create_before_destroy")
	}
}

func TestContext2Apply_moduleDependsOn(t *testing.T) {
	m := testModule(t, "apply-module-depends-on")

	p := testProvider("test")

	// each instance being applied should happen in sequential order
	applied := int64(0)

	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
		cfg := req.Config.AsValueMap()
		foo := cfg["foo"].AsString()
		ord := atomic.LoadInt64(&applied)

		resp := providers.ReadDataSourceResponse{
			State: cty.ObjectVal(map[string]cty.Value{
				"id":  cty.StringVal("data"),
				"foo": cfg["foo"],
			}),
		}

		if foo == "a" && ord < 4 {
			// due to data source "a"'s module depending on instance 4, this
			// should not be less than 4
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("data source a read too early"))
		}
		if foo == "b" && ord < 1 {
			// due to data source "b"'s module depending on instance 1, this
			// should not be less than 1
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("data source b read too early"))
		}
		return resp
	}
	p.DiffFn = testDiffFn

	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
		state := req.PlannedState.AsValueMap()
		num, _ := state["num"].AsBigFloat().Float64()
		ord := int64(num)
		if !atomic.CompareAndSwapInt64(&applied, ord-1, ord) {
			actual := atomic.LoadInt64(&applied)
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("instance %d was applied after %d", ord, actual))
		}

		state["id"] = cty.StringVal(fmt.Sprintf("test_%d", ord))
		resp.NewState = cty.ObjectVal(state)

		return resp
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	for _, res := range plan.Changes.Resources {
		if res.Action != plans.NoOp {
			t.Fatalf("expected NoOp, got %s for %s", res.Action, res.Addr)
		}
	}
}

func TestContext2Apply_moduleSelfReference(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
module "test" {
  source = "./test"

  a = module.test.b
}

output "c" {
  value = module.test.c
}
`,
		"test/main.tf": `
variable "a" {}

resource "test_instance" "test" {
}

output "b" {
  value = test_instance.test.id
}

output "c" {
  value = var.a
}`})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Destroy: true,
	})

	_, diags = ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	if !state.Empty() {
		t.Fatal("expected empty state, got:", state)
	}
}

func TestContext2Apply_moduleExpandDependsOn(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
module "child" {
  count = 1
  source = "./child"

  depends_on = [test_instance.a, test_instance.b]
}

resource "test_instance" "a" {
}


resource "test_instance" "b" {
}
`,
		"child/main.tf": `
resource "test_instance" "foo" {
}

output "myoutput" {
  value = "literal string"
}
`})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Destroy: true,
		State:   state,
	})

	_, diags = ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	if !state.Empty() {
		t.Fatal("expected empty state, got:", state)
	}
}

func TestContext2Apply_scaleInCBD(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
variable "ct" {
  type = number
}

resource "test_instance" "a" {
  count = var.ct
}

resource "test_instance" "b" {
  require_new = local.removable
  lifecycle {
	create_before_destroy = true
  }
}

resource "test_instance" "c" {
  require_new = test_instance.b.id
  lifecycle {
	create_before_destroy = true
  }
}

output "out" {
  value = join(".", test_instance.a[*].id)
}

locals {
  removable = join(".", test_instance.a[*].id)
}
`})

	state := states.NewState()
	root := state.EnsureModule(addrs.RootModuleInstance)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.a[0]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:              states.ObjectReady,
			AttrsJSON:           []byte(`{"id":"a0"}`),
			Dependencies:        []addrs.ConfigResource{},
			CreateBeforeDestroy: true,
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.a[1]").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:              states.ObjectReady,
			AttrsJSON:           []byte(`{"id":"a1"}`),
			Dependencies:        []addrs.ConfigResource{},
			CreateBeforeDestroy: true,
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.b").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:              states.ObjectReady,
			AttrsJSON:           []byte(`{"id":"b", "require_new":"old.old"}`),
			Dependencies:        []addrs.ConfigResource{mustConfigResourceAddr("test_instance.a")},
			CreateBeforeDestroy: true,
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)
	root.SetResourceInstanceCurrent(
		mustResourceInstanceAddr("test_instance.c").Resource,
		&states.ResourceInstanceObjectSrc{
			Status:    states.ObjectReady,
			AttrsJSON: []byte(`{"id":"c", "require_new":"b"}`),
			Dependencies: []addrs.ConfigResource{
				mustConfigResourceAddr("test_instance.a"),
				mustConfigResourceAddr("test_instance.b"),
			},
			CreateBeforeDestroy: true,
		},
		mustProviderConfig(`provider["registry.terraform.io/hashicorp/test"]`),
	)

	p := testProvider("test")
	p.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		return testApplyFn(info, s, d)
	}

	p.PlanResourceChangeFn = func(r providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
		n := r.ProposedNewState.AsValueMap()

		if r.PriorState.IsNull() {
			n["id"] = cty.UnknownVal(cty.String)
			resp.PlannedState = cty.ObjectVal(n)
			return resp
		}

		p := r.PriorState.AsValueMap()

		priorRN := p["require_new"]
		newRN := n["require_new"]

		if eq := priorRN.Equals(newRN); !eq.IsKnown() || eq.False() {
			resp.RequiresReplace = []cty.Path{{cty.GetAttrStep{Name: "require_new"}}}
			n["id"] = cty.UnknownVal(cty.String)
		}

		resp.PlannedState = cty.ObjectVal(n)
		return resp
	}

	// reduce the count to 1
	ctx := testContext2(t, &ContextOpts{
		Variables: InputValues{
			"ct": &InputValue{
				Value:      cty.NumberIntVal(1),
				SourceType: ValueFromCaller,
			},
		},
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	_, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		log.Fatal(diags.ErrWithWarnings())
	}

	// check the output, as those can't cause an error planning the value
	out := state.RootModule().OutputValues["out"].Value.AsString()
	if out != "a0" {
		t.Fatalf(`expected output "a0", got: %q`, out)
	}

	// reduce the count to 0
	ctx = testContext2(t, &ContextOpts{
		Variables: InputValues{
			"ct": &InputValue{
				Value:      cty.NumberIntVal(0),
				SourceType: ValueFromCaller,
			},
		},
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	_, diags = ctx.Plan()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	// check the output, as those can't cause an error planning the value
	out = state.RootModule().OutputValues["out"].Value.AsString()
	if out != "" {
		t.Fatalf(`expected output "", got: %q`, out)
	}
}

// Ensure that we can destroy when a provider references a resource that will
// also be destroyed
func TestContext2Apply_destroyProviderReference(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
provider "null" {
  value = ""
}

module "mod" {
  source = "./mod"
}

provider "test" {
  value = module.mod.output
}

resource "test_instance" "bar" {
}
`,
		"mod/main.tf": `
data "null_data_source" "foo" {
       count = 1
}


output "output" {
  value = data.null_data_source.foo[0].output
}
`})

	schemaFn := func(name string) *ProviderSchema {
		return &ProviderSchema{
			Provider: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"value": {
						Type:     cty.String,
						Required: true,
					},
				},
			},
			ResourceTypes: map[string]*configschema.Block{
				name + "_instance": {
					Attributes: map[string]*configschema.Attribute{
						"id": {
							Type:     cty.String,
							Computed: true,
						},
						"foo": {
							Type:     cty.String,
							Optional: true,
						},
					},
				},
			},
			DataSources: map[string]*configschema.Block{
				name + "_data_source": {
					Attributes: map[string]*configschema.Attribute{
						"id": {
							Type:     cty.String,
							Computed: true,
						},
						"output": {
							Type:     cty.String,
							Computed: true,
						},
					},
				},
			},
		}
	}

	testP := new(MockProvider)
	testP.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		return providers.ReadResourceResponse{NewState: req.PriorState}
	}
	testP.GetSchemaReturn = schemaFn("test")

	providerConfig := ""
	testP.ConfigureNewFn = func(req providers.ConfigureRequest) (resp providers.ConfigureResponse) {
		value := req.Config.GetAttr("value")
		if value.IsKnown() && !value.IsNull() {
			providerConfig = value.AsString()
		} else {
			providerConfig = ""
		}
		return resp
	}
	testP.ApplyFn = func(info *InstanceInfo, s *InstanceState, d *InstanceDiff) (*InstanceState, error) {
		if providerConfig != "valid" {
			return nil, fmt.Errorf("provider config is %q", providerConfig)
		}
		return testApplyFn(info, s, d)
	}
	testP.DiffFn = testDiffFn

	nullP := new(MockProvider)
	nullP.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		return providers.ReadResourceResponse{NewState: req.PriorState}
	}
	nullP.GetSchemaReturn = schemaFn("null")

	nullP.ApplyFn = testApplyFn
	nullP.DiffFn = testDiffFn

	nullP.ReadDataSourceResponse = providers.ReadDataSourceResponse{
		State: cty.ObjectVal(map[string]cty.Value{
			"id":     cty.StringVal("ID"),
			"output": cty.StringVal("valid"),
		}),
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(testP),
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(nullP),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(testP),
			addrs.NewDefaultProvider("null"): testProviderFuncFixed(nullP),
		},

		State:   state,
		Destroy: true,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("destroy plan errors: %s", diags.Err())
	}

	if _, diags := ctx.Apply(); diags.HasErrors() {
		t.Fatalf("destroy apply errors: %s", diags.Err())
	}
}

// Destroying properly requires pruning out all unneeded config nodes to
// prevent incorrect expansion evaluation.
func TestContext2Apply_destroyInterModuleExpansion(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
data "test_data_source" "a" {
  for_each = {
    one = "thing"
  }
}

locals {
  module_input = {
    for k, v in data.test_data_source.a : k => v.id
  }
}

module "mod1" {
  source = "./mod"
  input = local.module_input
}

module "mod2" {
  source = "./mod"
  input = module.mod1.outputs
}

resource "test_instance" "bar" {
  for_each = module.mod2.outputs
}

output "module_output" {
  value = module.mod2.outputs
}
output "test_instances" {
  value = test_instance.bar
}
`,
		"mod/main.tf": `
variable "input" {
}

data "test_data_source" "foo" {
  for_each = var.input
}

output "outputs" {
  value = data.test_data_source.foo
}
`})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn
	p.ReadDataSourceFn = func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
		return providers.ReadDataSourceResponse{
			State: cty.ObjectVal(map[string]cty.Value{
				"id":  cty.StringVal("data_source"),
				"foo": cty.StringVal("output"),
			}),
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	destroy := func() {
		ctx = testContext2(t, &ContextOpts{
			Config: m,
			Providers: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
			},

			State:   state,
			Destroy: true,
		})

		if _, diags := ctx.Plan(); diags.HasErrors() {
			t.Fatalf("destroy plan errors: %s", diags.Err())
		}

		state, diags = ctx.Apply()
		if diags.HasErrors() {
			t.Fatalf("destroy apply errors: %s", diags.Err())
		}
	}

	destroy()
	// Destroying again from the empty state should not cause any errors either
	destroy()
}

func TestContext2Apply_createBeforeDestroyWithModule(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
variable "v" {}

module "mod" {
    source = "./mod"
    in = var.v
}

resource "test_resource" "a" {
  value = var.v
  depends_on = [module.mod]
  lifecycle {
    create_before_destroy = true
  }
}
`,
		"mod/main.tf": `
variable "in" {}

resource "test_resource" "a" {
  value = var.in
}
`})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		proposed := req.ProposedNewState.AsValueMap()
		proposed["id"] = cty.UnknownVal(cty.String)
		return providers.PlanResourceChangeResponse{
			PlannedState:    cty.ObjectVal(proposed),
			RequiresReplace: []cty.Path{cty.Path{cty.GetAttrStep{Name: "value"}}},
		}
	}

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"v": &InputValue{
				Value: cty.StringVal("A"),
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"v": &InputValue{
				Value: cty.StringVal("B"),
			},
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_forcedCBD(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
variable "v" {}

resource "test_instance" "a" {
  require_new = var.v
}

resource "test_instance" "b" {
  depends_on = [test_instance.a]
  lifecycle {
    create_before_destroy = true
  }
}
`})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"v": &InputValue{
				Value: cty.StringVal("A"),
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"v": &InputValue{
				Value: cty.StringVal("B"),
			},
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_removeReferencedResource(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
variable "ct" {
}

resource "test_resource" "to_remove" {
  count = var.ct
}

resource "test_resource" "c" {
  value = join("", test_resource.to_remove[*].id)
}
`})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"ct": &InputValue{
				Value: cty.NumberIntVal(1),
			},
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"ct": &InputValue{
				Value: cty.NumberIntVal(0),
			},
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_variableSensitivity(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
terraform {
	experiments = [sensitive_variables]
}

variable "sensitive_var" {
	default = "foo"
	sensitive = true
}

variable "sensitive_id" {
	default = "secret id"
	sensitive = true
}

resource "test_resource" "foo" {
	value   = var.sensitive_var

	network_interface {
		network_interface_id = var.sensitive_id
	}
}`,
	})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	// Run a second apply with no changes
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	state, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	// Now change the variable value for sensitive_var
	ctx = testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
		Variables: InputValues{
			"sensitive_var": &InputValue{
				Value: cty.StringVal("bar"),
			},
		},
		State: state,
	})

	if _, diags := ctx.Plan(); diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	_, diags = ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}
}

func TestContext2Apply_variableSensitivityPropagation(t *testing.T) {
	m := testModuleInline(t, map[string]string{
		"main.tf": `
terraform {
	experiments = [sensitive_variables]
}

variable "sensitive_map" {
	type = map(string)
	default = {
		"x" = "foo"
	}
	sensitive = true
}

resource "test_resource" "foo" {
	value = var.sensitive_map.x
}`,
	})

	p := testProvider("test")
	p.ApplyFn = testApplyFn
	p.DiffFn = testDiffFn

	ctx := testContext2(t, &ContextOpts{
		Config: m,
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		},
	})

	plan, diags := ctx.Plan()
	if diags.HasErrors() {
		t.Fatalf("plan errors: %s", diags.Err())
	}

	verifySensitiveValue := func(pvms []cty.PathValueMarks) {
		if len(pvms) != 1 {
			t.Fatalf("expected 1 sensitive path, got %d", len(pvms))
		}
		pvm := pvms[0]
		if gotPath, wantPath := pvm.Path, cty.GetAttrPath("value"); !gotPath.Equals(wantPath) {
			t.Errorf("wrong path\n got: %#v\nwant: %#v", gotPath, wantPath)
		}
		if gotMarks, wantMarks := pvm.Marks, cty.NewValueMarks("sensitive"); !gotMarks.Equal(wantMarks) {
			t.Errorf("wrong marks\n got: %#v\nwant: %#v", gotMarks, wantMarks)
		}
	}

	addr := mustResourceInstanceAddr("test_resource.foo")

	fooChangeSrc := plan.Changes.ResourceInstance(addr)
	verifySensitiveValue(fooChangeSrc.AfterValMarks)

	state, diags := ctx.Apply()
	if diags.HasErrors() {
		t.Fatalf("apply errors: %s", diags.Err())
	}

	fooState := state.ResourceInstance(addr)
	verifySensitiveValue(fooState.Current.AttrSensitivePaths)
}
