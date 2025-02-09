package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"github.com/manifoldco/promptui"
	"github.com/simontheleg/konf-go/config"
	"github.com/simontheleg/konf-go/testhelper"
	"github.com/simontheleg/konf-go/utils"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

func TestSelectLastKonf(t *testing.T) {
	fm := testhelper.FilesystemManager{}

	tt := map[string]struct {
		InFs     afero.Fs
		ExpID    string
		ExpError error
	}{
		"latestKonf set": {
			InFs:     testhelper.FSWithFiles(fm.LatestKonf),
			ExpID:    "context_cluster",
			ExpError: nil,
		},
		"no latestKonf": {
			InFs:     testhelper.FSWithFiles(),
			ExpID:    "",
			ExpError: fmt.Errorf("could not select latest konf, because no konf was yet set"),
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {
			id, err := selectLastKonf(tc.InFs)

			if !testhelper.EqualError(tc.ExpError, err) {
				t.Errorf("Want error %q, got %q", tc.ExpError, err)
			}

			if tc.ExpID != id {
				t.Errorf("Want ID %q, got %q", tc.ExpID, id)
			}
		})
	}
}

func TestCompleteSet(t *testing.T) {
	// since cobra takes care of the majority of the complexity (like parsing out results that don't match completion start),
	// we only need to test regular cases
	fm := testhelper.FilesystemManager{}

	tt := map[string]struct {
		fs           afero.Fs
		expComp      []string
		expCompDirec cobra.ShellCompDirective
	}{
		"normal results": {
			testhelper.FSWithFiles(fm.StoreDir, fm.SingleClusterSingleContextASIA, fm.SingleClusterSingleContextEU),
			[]string{"dev-asia_dev-asia-1", "dev-eu_dev-eu-1"},
			cobra.ShellCompDirectiveNoFileComp,
		},
		"no results": {
			testhelper.FSWithFiles(fm.StoreDir),
			[]string{},
			cobra.ShellCompDirectiveNoFileComp,
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {
			scmd := newSetCommand()
			scmd.fs = tc.fs

			res, compdirec := scmd.completeSet(scmd.cmd, []string{}, "")

			if !cmp.Equal(res, tc.expComp) {
				t.Errorf("Exp and given comps differ: \n '%s'", cmp.Diff(tc.expComp, res))
			}

			if compdirec != tc.expCompDirec {
				t.Errorf("Exp compdirec %q, got %q", tc.expCompDirec, compdirec)
			}
		})
	}
}

func TestSaveLatestKonf(t *testing.T) {
	expFile := "./konf/latestkonf"
	expID := "context_cluster"

	f := afero.NewMemMapFs()
	err := saveLatestKonf(f, expID)
	if err != nil {
		t.Errorf("Could not save last konf: %q", err)
	}
	finf, err := f.Stat(expFile)
	if err != nil {
		t.Errorf("Could not stat file: %q", err)
	}
	if finf == nil {
		t.Errorf("Exp file %q to be present, but it isnt", expFile)
	}
	id, _ := afero.ReadFile(f, expFile)
	if string(id) != expID {
		t.Errorf("Exp id to be %q but is %q", expID, id)
	}
}

func TestSetContext(t *testing.T) {
	storeDir := config.StoreDir()
	ppid := os.Getppid()
	sm := testhelper.SampleKonfManager{}

	tt := map[string]struct {
		InID        string
		StoreExists bool
		ExpErr      error
		ExpKonfPath string
	}{
		"normal write": {
			"dev-eu_dev-eu",
			true,
			nil,
			utils.ActivePathForID(fmt.Sprint(ppid)),
		},
		"invalid id": {
			"i-am-invalid",
			false,
			fs.ErrNotExist,
			"",
		},
	}

	for name, tc := range tt {

		t.Run(name, func(t *testing.T) {
			f := afero.NewMemMapFs()

			if tc.StoreExists {
				afero.WriteFile(f, storeDir+"/"+tc.InID+".yaml", []byte(sm.SingleClusterSingleContextEU()), utils.KonfPerm)
			}

			resKonfPath, resError := setContext(tc.InID, f)

			if !errors.Is(resError, tc.ExpErr) {
				t.Errorf("Want error '%s', got '%s'", tc.ExpErr, resError)
			}

			if resKonfPath != tc.ExpKonfPath {
				t.Errorf("Want konfPath '%s', got '%s'", tc.ExpKonfPath, resKonfPath)
			}

			if tc.ExpKonfPath != "" {
				_, err := f.Stat(tc.ExpKonfPath)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						t.Errorf("Exp file %q to be present, but it is not", tc.ExpKonfPath)
					} else {
						t.Fatalf("Unexpected error occurred: '%s'", err)
					}
				}
				res, err := afero.ReadFile(f, tc.ExpKonfPath)
				if err != nil {
					t.Errorf("Wanted to read file %q, but failed: %q", tc.ExpKonfPath, err)
				}
				if string(res) != sm.SingleClusterSingleContextEU() {
					t.Errorf("Exp content %q, got %q", res, sm.SingleClusterSingleContextEU())
				}
			}
		})

	}
}

func TestPrepareTemplates(t *testing.T) {
	tt := map[string]struct {
		Values      tableOutput
		Trunc       int
		ExpInactive string
		ExpActive   string
		ExpLabel    string
	}{
		"values < trunc": {
			tableOutput{
				"kind-eu",
				"cluster-eu",
				"kind-eu.cluster-eu.yaml",
			},
			25,
			"  kind-eu                   | cluster-eu                | kind-eu.cluster-eu.yaml   |",
			"▸ kind-eu                   | cluster-eu                | kind-eu.cluster-eu.yaml   |",
			"  Context                   | Cluster                   | File                      ",
		},
		"values == trunc": {
			tableOutput{
				"0123456789",
				"0123456789",
				"xyz.yaml",
			},
			10,
			"  0123456789 | 0123456789 | xyz.yaml   |",
			"▸ 0123456789 | 0123456789 | xyz.yaml   |",
			"  Context    | Cluster    | File       ",
		},
		"values > trunc": {
			tableOutput{
				"0123456789-andlotsmore",
				"0123456789-andlotsmore",
				"xyz.yaml",
			},
			10,
			"  0123456789 | 0123456789 | xyz.yaml   |",
			"▸ 0123456789 | 0123456789 | xyz.yaml   |",
			"  Context    | Cluster    | File       ",
		},
		"trunc is below minLength": {
			tableOutput{
				"0123456789",
				"0123456789",
				"xyz.yaml",
			},
			5,
			"  0123456 | 0123456 | xyz.yam |",
			"▸ 0123456 | 0123456 | xyz.yam |",
			"  Context | Cluster | File    ",
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {
			inactive, active, label := prepareTable(tc.Trunc)

			checkTemplate(t, inactive, tc.Values, tc.ExpInactive)
			checkTemplate(t, active, tc.Values, tc.ExpActive)
			checkTemplate(t, label, tc.Values, tc.ExpLabel)
		})
	}
}

func checkTemplate(t *testing.T, stpl string, val tableOutput, exp string) {

	tmpl, err := template.New("t").Funcs(newTemplateFuncMap()).Parse(stpl)
	if err != nil {
		t.Fatalf("Could not create template for test '%v'. Please check test code", err)
	}

	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, val)
	if err != nil {
		t.Fatalf("Could not execute template for test '%v'. Please check test code", err)
	}

	res := buf.String()
	// remove any formatting as we do not care about that
	cyan := "\x1b[36m"
	bold := "\x1b[1m"
	normal := "\x1b[0m"
	res = strings.Replace(res, cyan, "", -1)
	res = strings.Replace(res, bold, "", -1)
	res = strings.Replace(res, normal, "", -1)
	if exp != res {
		t.Errorf("Exp res: '%s', got: '%s'", exp, res)
	}
}

func TestFetchKonfs(t *testing.T) {
	fm := testhelper.FilesystemManager{}

	tt := map[string]struct {
		FSIn        afero.Fs
		CheckError  func(*testing.T, error) // currently this convoluted mess is needed so we can accurately check for types. errors.As does not work in our case
		ExpTableOut []tableOutput
	}{
		"empty store": {
			FSIn:        testhelper.FSWithFiles(fm.StoreDir),
			CheckError:  expEmptyStore,
			ExpTableOut: nil,
		},
		"valid konfs and a wrong konf": {
			FSIn:       testhelper.FSWithFiles(fm.StoreDir, fm.SingleClusterSingleContextEU, fm.SingleClusterSingleContextASIA, fm.InvalidYaml),
			CheckError: expNil,
			ExpTableOut: []tableOutput{
				{
					Context: "dev-asia",
					Cluster: "dev-asia-1",
					File:    "./konf/store/dev-asia_dev-asia-1.yaml",
				},
				{
					Context: "dev-eu",
					Cluster: "dev-eu-1",
					File:    "./konf/store/dev-eu_dev-eu-1.yaml",
				},
			},
		},
		"overloaded konf (cluster)": {
			FSIn:        testhelper.FSWithFiles(fm.StoreDir, fm.MultiClusterSingleContext),
			CheckError:  expKubeConfigOverload,
			ExpTableOut: nil,
		},
		"overloaded konf (context)": {
			FSIn:        testhelper.FSWithFiles(fm.StoreDir, fm.SingleClusterMultiContext),
			CheckError:  expKubeConfigOverload,
			ExpTableOut: nil,
		},
		"the nice MacOS .DS_Store file": {
			FSIn:       testhelper.FSWithFiles(fm.StoreDir, fm.DSStore, fm.SingleClusterSingleContextEU),
			CheckError: expNil,
			ExpTableOut: []tableOutput{
				{
					Context: "dev-eu",
					Cluster: "dev-eu-1",
					File:    "./konf/store/dev-eu_dev-eu-1.yaml",
				},
			},
		},
		"ignore directories": {
			FSIn:       testhelper.FSWithFiles(fm.StoreDir, fm.SingleClusterSingleContextEU, fm.EmptyDir),
			CheckError: expNil,
			ExpTableOut: []tableOutput{
				{
					Context: "dev-eu",
					Cluster: "dev-eu-1",
					File:    "./konf/store/dev-eu_dev-eu-1.yaml",
				},
			},
		},
		"only directories in store": {
			FSIn:        testhelper.FSWithFiles(fm.StoreDir, fm.EmptyDir),
			CheckError:  expEmptyStore,
			ExpTableOut: nil,
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {
			out, err := fetchKonfs(tc.FSIn)

			tc.CheckError(t, err)

			if !cmp.Equal(tc.ExpTableOut, out) {
				t.Errorf("Exp and given tableoutputs differ:\n'%s'", cmp.Diff(tc.ExpTableOut, out))
			}
		})
	}
}

func TestSelectContext(t *testing.T) {
	fm := testhelper.FilesystemManager{}
	f := testhelper.FSWithFiles(fm.StoreDir, fm.SingleClusterSingleContextEU, fm.SingleClusterSingleContextASIA)

	// cases
	// - invalid selection
	// - prompt failure
	tt := map[string]struct {
		pf     promptFunc
		expID  string
		expErr error
	}{
		"select asia": {
			func(s *promptui.Select) (int, error) { return 0, nil },
			"dev-asia_dev-asia-1",
			nil,
		},
		"select eu": {
			func(s *promptui.Select) (int, error) { return 1, nil },
			"dev-eu_dev-eu-1",
			nil,
		},
		"prompt failure": {
			func(s *promptui.Select) (int, error) { return 1, fmt.Errorf("err") },
			"",
			fmt.Errorf("err"),
		},
		"invalid selection": {
			func(s *promptui.Select) (int, error) { return 2, nil },
			"",
			fmt.Errorf("invalid selection 2"),
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {

			res, err := selectContext(f, tc.pf)

			if !testhelper.EqualError(err, tc.expErr) {
				t.Errorf("Exp err %q, got %q", tc.expErr, err)
			}

			if res != tc.expID {
				t.Errorf("Exp id %q, got %q", tc.expID, res)
			}
		})
	}
}

func expEmptyStore(t *testing.T, err error) {
	if _, ok := err.(*EmptyStore); !ok {
		t.Errorf("Expected err to be of type EmptyStore")
	}
}

func expKubeConfigOverload(t *testing.T, err error) {
	if _, ok := err.(*KubeConfigOverload); !ok {
		t.Errorf("Expected err to be of type KubeConfigOverload")
	}
}

func expNil(t *testing.T, err error) {
	if err != nil {
		t.Errorf("Expected err to be nil, but got %q", err)
	}
}

func TestSearchKonf(t *testing.T) {
	tt := map[string]struct {
		search string
		item   *tableOutput
		expRes bool
	}{
		"full match across all": {
			"a b c",
			&tableOutput{"a", "b", "c"},
			true,
		},
		"full match across all - fuzzy": {
			"abc",
			&tableOutput{"a", "b", "c"},
			true,
		},
		"partial match across fields": {
			"textclu",
			&tableOutput{"context", "cluster", "file"},
			true,
		},
		"no match": {
			"oranges",
			&tableOutput{"apples", "and", "bananas"},
			false,
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {
			res := searchKonf(tc.search, tc.item)
			if res != tc.expRes {
				t.Errorf("Exp res to be %t got %t", tc.expRes, res)
			}
		})
	}
}
