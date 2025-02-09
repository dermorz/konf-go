package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	sprig "github.com/Masterminds/sprig/v3"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/manifoldco/promptui"
	"github.com/simontheleg/konf-go/config"
	log "github.com/simontheleg/konf-go/log"
	"github.com/simontheleg/konf-go/prompt"
	"github.com/simontheleg/konf-go/utils"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	k8s "k8s.io/client-go/tools/clientcmd/api/v1"
	"sigs.k8s.io/yaml"
)

type setCmd struct {
	fs afero.Fs

	cmd *cobra.Command
}

func newSetCommand() *setCmd {

	sc := &setCmd{
		fs: afero.NewOsFs(),
	}

	sc.cmd = &cobra.Command{
		Use:   `set`,
		Short: "Set kubeconfig to use in current shell",
		Args:  cobra.MaximumNArgs(1),
		Long: `Sets kubeconfig to use or start picker dialogue.
	
	Examples:
		-> 'set' run konf selection
		-> 'set <konfig id>' set a specific konf
		-> 'set -' set to last used konf
	`,
		RunE:              sc.set,
		ValidArgsFunction: sc.completeSet,
	}

	return sc
}

func (c *setCmd) set(cmd *cobra.Command, args []string) error {
	// TODO if I stay with the mocking approach used in commands like
	// namespace. This part should be refactored to allow for mocking
	// the downstream funcs in order to test the if-else logic
	var id string
	var err error

	if len(args) == 0 {
		id, err = selectContext(c.fs, prompt.Terminal)
		if err != nil {
			return err
		}
	} else if args[0] == "-" {
		id, err = selectLastKonf(c.fs)
		if err != nil {
			return err
		}
	} else {
		id = args[0]
	}

	context, err := setContext(id, c.fs)
	if err != nil {
		return err
	}
	err = saveLatestKonf(c.fs, id)
	if err != nil {
		return fmt.Errorf("could not save latest konf. As a result 'konf set -' might not work: %q ", err)
	}

	log.Info("Setting context to %q\n", id)

	// By printing out to stdout, we pass the value to our zsh hook, which then sets $KUBECONFIG to it
	// Both operate on the convention to use "KUBECONFIGCHANGE:<new-path>". If you change this part in
	// here, do not forget to update shellwraper.go
	fmt.Println("KUBECONFIGCHANGE:" + context)

	return nil
}

func (c *setCmd) completeSet(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	konfs, err := fetchKonfs(c.fs)
	if err != nil {
		// if the store is just empty, return no suggestions, instead of throwing an error
		if _, ok := err.(*EmptyStore); ok {
			return []string{}, cobra.ShellCompDirectiveNoFileComp
		}

		cobra.CompDebugln(err.Error(), true)
		return nil, cobra.ShellCompDirectiveError
	}

	sug := []string{}
	for _, konf := range konfs {
		// with the current design of 'set', we need to return the ID here in the autocomplete as the first part of the completion
		// as it is directly passed to set
		sug = append(sug, utils.IDFromClusterAndContext(konf.Cluster, konf.Context))
	}

	return sug, cobra.ShellCompDirectiveNoFileComp
}

type promptFunc func(*promptui.Select) (int, error)

func selectContext(f afero.Fs, pf promptFunc) (string, error) {
	k, err := fetchKonfs(f)
	if err != nil {
		return "", err
	}
	p := createPrompt(k)
	selPos, err := pf(p)
	if err != nil {
		return "", err
	}

	if selPos >= len(k) {
		return "", fmt.Errorf("invalid selection %d", selPos)
	}
	sel := k[selPos]

	return utils.IDFromClusterAndContext(sel.Cluster, sel.Context), nil
}

func selectLastKonf(f afero.Fs) (string, error) {
	b, err := afero.ReadFile(f, config.LatestKonfFile())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("could not select latest konf, because no konf was yet set")
		}
		return "", err
	}
	return string(b), nil
}

func setContext(id string, f afero.Fs) (string, error) {
	konf, err := afero.ReadFile(f, utils.StorePathForID(id))
	if err != nil {
		return "", err
	}

	ppid := os.Getppid()
	activeKonf := utils.ActivePathForID(fmt.Sprint(ppid))
	err = afero.WriteFile(f, activeKonf, konf, utils.KonfPerm)
	if err != nil {
		return "", err
	}

	return activeKonf, nil

}

func saveLatestKonf(f afero.Fs, id string) error {
	return afero.WriteFile(f, config.LatestKonfFile(), []byte(id), utils.KonfPerm)
}

// KubeConfigOverload describes a state in which a kubeconfig has multiple Contexts or Clusters
// This can be undesirable for konf when such a kubeconfig is in its store
type KubeConfigOverload struct {
	path string
}

func (k *KubeConfigOverload) Error() string {
	return fmt.Sprintf("Impure Store: The kubeconfig %q contains multiple contexts and/or clusters. Please only use 'konf import' for populating the store\n", k.path)
}

// EmptyStore describes a state in which no kubeconfig is inside the store
// It makes sense to have this in a separate case as it does not matter for some operations (e.g. importing) but detrimental for others (e.g. running the selection prompt)
type EmptyStore struct{}

func (k *EmptyStore) Error() string {
	return fmt.Sprintf("The konf store at %q is empty. Please run 'konf import' to populate it", config.StoreDir())
}

// fetchKonfs returns a list of all konfs currently in konfDir/store. Additionally it returns metadata on these konfs for easier usage of the information
func fetchKonfs(f afero.Fs) ([]tableOutput, error) {
	var konfs []fs.FileInfo

	err := afero.Walk(f, config.StoreDir(), func(path string, info fs.FileInfo, err error) error {
		// do not add directories. This is important as later we check the number of items in konf to determine whether store is empty or not
		// without this check we would display an empty prompt if the user has only directories in their storeDir
		if info.IsDir() && path != config.StoreDir() {
			return filepath.SkipDir
		}

		// skip any hidden files
		if strings.HasPrefix(info.Name(), ".") {
			// I have decided to not print any log line on this, which differs from the logic
			// for malformed kubeconfigs. I think this makes sense as konf import will never produce
			// a hidden file and the purpose of this check is rather to protect against
			// automatically created files like the .DS_Store on MacOs. On the other side however
			// it is quite easy to create a malformed kubeconfig without noticing
			return nil
		}

		konfs = append(konfs, info)
		return nil
	})

	if err != nil {
		return nil, err
	}

	// cut out the root element, which gets added in the previous step
	// this is safe as the element is guaranteed to be at the first position
	konfs = konfs[1:]

	// similar to fs.ReadDir, sort the entries for easier viewing for the user and to
	// be consistent with what shells return during auto-completion
	sort.Slice(konfs, func(i, j int) bool { return konfs[i].Name() < konfs[j].Name() })

	if len(konfs) == 0 {
		return nil, &EmptyStore{}
	}

	out := []tableOutput{}
	// TODO the logic of this loop should be extracted into the walkFn above to avoid looping twice
	// TODO (possibly the walkfunction should also be extracted into its own function)
	for _, konf := range konfs {

		id := utils.IDFromFileInfo(konf)
		path := utils.StorePathForID(id)
		file, err := f.Open(path)
		if err != nil {
			return nil, err
		}
		val, err := afero.ReadAll(file)
		if err != nil {
			return nil, err
		}
		kubeconf := &k8s.Config{}
		err = yaml.Unmarshal(val, kubeconf)
		if err != nil {
			log.Warn("file %q does not contain a valid kubeconfig. Skipping for evaluation", path)
			continue
		}

		if len(kubeconf.Contexts) > 1 || len(kubeconf.Clusters) > 1 {
			// This directly returns, as an impure store is a danger for other usage down the road
			return nil, &KubeConfigOverload{path}
		}

		t := tableOutput{}
		t.Context = kubeconf.Contexts[0].Name
		t.Cluster = kubeconf.Clusters[0].Name
		t.File = path
		out = append(out, t)
	}
	return out, nil
}

func createPrompt(options []tableOutput) *promptui.Select {
	// TODO use ssh/terminal to get the terminalsize and set trunc accordingly https://stackoverflow.com/questions/16569433/get-terminal-size-in-go
	trunc := 25
	promptInactive, promptActive, label := prepareTable(trunc)

	// Wrapper is required as we need access to options, but the methodSignature from promptUI
	// requires you to only pass an index not the whole func
	// This wrapper allows us to unit-test the searchKonf func better
	var wrapSearchKonf = func(input string, index int) bool {
		return searchKonf(input, &options[index])
	}

	prompt := promptui.Select{
		Label: label,
		Items: options,
		Templates: &promptui.SelectTemplates{
			Active:   promptActive,
			Inactive: promptInactive,
			FuncMap:  newTemplateFuncMap(),
		},
		HideSelected: true,
		Stdout:       os.Stderr,
		Searcher:     wrapSearchKonf,
		Size:         15,
	}
	return &prompt
}

func searchKonf(searchTerm string, curItem *tableOutput) bool {
	// since there is no weight on any of the table entries, we can just combine them to one string
	// and run the contains on it, which automatically is going to match any of the three values
	r := fmt.Sprintf("%s %s %s", curItem.Context, curItem.Cluster, curItem.File)
	return fuzzy.Match(searchTerm, r)
}

// TODO only inject the funcs I am actually using
func newTemplateFuncMap() template.FuncMap {
	ret := sprig.TxtFuncMap()
	ret["black"] = promptui.Styler(promptui.FGBlack)
	ret["red"] = promptui.Styler(promptui.FGRed)
	ret["green"] = promptui.Styler(promptui.FGGreen)
	ret["yellow"] = promptui.Styler(promptui.FGYellow)
	ret["blue"] = promptui.Styler(promptui.FGBlue)
	ret["magenta"] = promptui.Styler(promptui.FGMagenta)
	ret["cyan"] = promptui.Styler(promptui.FGCyan)
	ret["white"] = promptui.Styler(promptui.FGWhite)
	ret["bgBlack"] = promptui.Styler(promptui.BGBlack)
	ret["bgRed"] = promptui.Styler(promptui.BGRed)
	ret["bgGreen"] = promptui.Styler(promptui.BGGreen)
	ret["bgYellow"] = promptui.Styler(promptui.BGYellow)
	ret["bgBlue"] = promptui.Styler(promptui.BGBlue)
	ret["bgMagenta"] = promptui.Styler(promptui.BGMagenta)
	ret["bgCyan"] = promptui.Styler(promptui.BGCyan)
	ret["bgWhite"] = promptui.Styler(promptui.BGWhite)
	ret["bold"] = promptui.Styler(promptui.FGBold)
	ret["faint"] = promptui.Styler(promptui.FGFaint)
	ret["italic"] = promptui.Styler(promptui.FGItalic)
	ret["underline"] = promptui.Styler(promptui.FGUnderline)
	return ret
}

// tableOutput describes a formatting of kubekonf information, that is being used to present the user a nice table selection
type tableOutput struct {
	// Since we have no other use for structured information, we can safely leave this in set.go for now
	Context string
	Cluster string
	File    string
}

// prepareTable takes in the max length of each column and returns table rows for active, inactive and header
func prepareTable(maxColumnLen int) (inactive, active, label string) {
	// minColumnLen is determined by the length of the largest word in the label line
	minColumnLen := 7
	if maxColumnLen < minColumnLen {
		maxColumnLen = minColumnLen
	}
	// TODO figure out if we can do abbreviation using '...' somehow
	inactive = fmt.Sprintf(`  {{ repeat %[1]d " " | print .Context | trunc %[1]d | %[2]s }} | {{ repeat %[1]d " " | print .Cluster | trunc %[1]d | %[2]s }} | {{ repeat %[1]d  " " | print .File | trunc %[1]d | %[2]s }} |`, maxColumnLen, "")
	active = fmt.Sprintf(`▸ {{ repeat %[1]d " " | print .Context | trunc %[1]d | %[2]s }} | {{ repeat %[1]d " " | print .Cluster | trunc %[1]d | %[2]s }} | {{ repeat %[1]d  " " | print .File | trunc %[1]d | %[2]s }} |`, maxColumnLen, "bold | cyan")
	label = fmt.Sprint("  Context" + strings.Repeat(" ", maxColumnLen-7) + " | " + "Cluster" + strings.Repeat(" ", maxColumnLen-7) + " | " + "File" + strings.Repeat(" ", maxColumnLen-4) + " ") // repeat = trunc - length of the word before it
	return inactive, active, label
}

func init() {
	rootCmd.AddCommand(newSetCommand().cmd)
}
