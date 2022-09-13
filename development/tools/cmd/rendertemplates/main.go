package main

import (
	"bytes"
	"encoding/gob"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig"
	"github.com/google/go-github/v42/github"
	"github.com/imdario/mergo"
	"github.com/kyma-project/test-infra/development/github/pkg/client"
	rt "github.com/kyma-project/test-infra/development/tools/pkg/rendertemplates"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// autogenerationMessage is message added at the beginning of each autogenerated file.
	autogenerationMessage = "Code generated by rendertemplates. DO NOT EDIT."
	// configGithubOrg is a default organisation name to get rendertemplate from GitHub
	configGithubOrg = "kyma-project"
	// configGithubRepo is a default repository name to get rendertemplate from GitHub
	configGithubRepo = "test-infra"
	// configGithubPath is a default path to get rendertemplate from GitHub
	configGithubPath = "templates/config.yaml"
	// templatesDirGithubPath is a default templates directory path to get templates from GitHub
	templatesDirGithubPath = "templates/templates"
)

var (
	configFilePath   = flag.String("config", "", "Path to the config file.")
	dataDirPath      = flag.String("data", ".", "Path to the data directory.")
	dataFilePath     = flag.String("data-file", "", "Path to the data file.")
	templatesDirPath = flag.String("templates", "", "Path to the templates directory.")
	showOutputDir    = flag.Bool("show-output-dir", false, "Print generated output file paths to stdout")
	ghToken          = flag.String("gh-token", "", "GitHub Access Token")

	ghClient   *github.Client
	configFile []byte
	err        error
	dataFiles  []string

	additionalFuncs = map[string]interface{}{
		"matchingReleases": rt.MatchingReleases,
		"releaseMatches":   rt.ReleaseMatches,
		"hasPresubmit":     hasPresubmit,
		"hasPostsubmit":    hasPostsubmit,
		"hasPeriodic":      hasPeriodic,
		"getRunId":         getRunID,
	}
	commentSignByFileExt = map[string]sets.String{
		"//": sets.NewString(".go"),
		"> ": sets.NewString(".md"),
		"#":  sets.NewString(".yaml", ".yml"),
	}
)

func init() {
	gob.Register(map[string]interface{}{})
	gob.Register(map[interface{}]interface{}{})
	gob.Register([]interface{}{})
}

func main() {
	mergoConfig := mergo.Config{}
	// templatesCache stores already downloaded templates from GitHub to decrease API calls and prevent hit a rate limits.
	templatesCache := make(map[string]*template.Template)
	ctx := context.Background()

	flag.BoolVar(&mergoConfig.AppendSlice, "append-slice", false, "Rendertemplate will append slices instead overwriting.")
	flag.Parse()

	if *ghToken != "" {
		ghc, err := client.NewClient(ctx, *ghToken)
		if err != nil {
			log.Fatalf("Failed create authenticated GitHub Client: error: %s:", err.Error())
		}
		ghClient = ghc.Client
	} else {
		ghClient = github.NewClient(nil)
	}

	if *configFilePath == "" {
		// read rendertemplate config file from github
		// rendertemplate config contains global configsets
		configFile, err = getConfigFromGithub(ctx, ghClient)
		if err != nil {
			log.Fatalf("Failed load rendertemplate config file from github.com/%s/%s/%s, error: %s", configGithubOrg, configGithubRepo, configGithubPath, err.Error())
		}
	} else {
		// read rendertemplate config file from local filesystem
		// rendertemplate config contains global configsets
		configFile, err = os.ReadFile(*configFilePath)
		if err != nil {
			log.Fatalf("Cannot read config file from local filesystem: %s", err.Error())
		}
	}

	rtConfig := new(rt.Config)
	err = yaml.Unmarshal(configFile, rtConfig)
	if err != nil {
		log.Fatalf("Cannot parse config yaml: %s\n", err.Error())
	}

	if *dataFilePath != "" {
		// read only provided data file
		dataFiles = append(dataFiles, *dataFilePath)
		*dataDirPath = path.Dir(*dataFilePath)
	} else if *dataFilePath == "" && *dataDirPath != "" {
		// read all template data from data files
		err = filepath.Walk(*dataDirPath, getFileWalkFunc(*dataDirPath, &dataFiles))
		if err != nil {
			log.Fatalf("Cannot read data file directory: %s", err)
		}
	} else {
		log.Fatalf("Cannot read data file directory: %s", err)
	}

	// var dataFilesTemplates []*rt.TemplateConfig
	for _, dataFile := range dataFiles {
		var dataFileConfig rt.Config
		var cfg bytes.Buffer
		// Load datafile as template.
		t, err := loadTemplate(dataFile, templatesCache)
		if err != nil {
			log.Fatalf("Could not load data file %s: %v", dataFile, err)
		}
		// Execute rendering the datafile from datafile itself as a template and config as data.
		// Store it in-memory. At this point the config has all the global values from config.yaml file.
		// We do this in case a datafile to generate prowjobs definitions is itself a template, thus
		// it contains golang template actions. We execute a datafile as template with config as datafile to set
		// some datafile values from config global values. This is used for generating prowjobs for supported
		// releases only. Config global values provide list of supported releases. This is used as data to render
		// datafiles containing only supported releases versions as data.
		// This rendered datafiles are then used to render prowjobs definitions, by applying prowjob definition
		// template to them.
		// If datafile doesn't contain any golang templates actions, output will be just a datafile itself.
		if err := t.Execute(&cfg, rtConfig); err != nil {
			log.Fatalf("Cannot render data template: %v", err)
		}
		if err := yaml.Unmarshal(cfg.Bytes(), &dataFileConfig); err != nil {
			log.Fatalf("Cannot parse data file %s: %s\n", dataFile, err)
		}
		// append all generated configs from datafile to the list of templates to generate jobs from
		rtConfig.TemplatesConfigs = append(rtConfig.TemplatesConfigs, dataFileConfig.TemplatesConfigs...)
	}

	rtConfig.Merge(mergoConfig)

	// generate final .yaml files
	for _, templateConfig := range rtConfig.TemplatesConfigs {
		err = renderTemplate(*dataDirPath, templateConfig, rtConfig, templatesCache)
		if err != nil {
			log.Fatalf("Cannot render template %s: %s", templateConfig.From, err)
		}
	}
}

// getConfigFromGithub downloads rendertemplate config from GitHub.
// It uses default location in test-infra repository.
func getConfigFromGithub(ctx context.Context, ghClient *github.Client) ([]byte, error) {
	// ctx := context.Background()
	configFileContent, _, resp, err := ghClient.Repositories.GetContents(ctx, configGithubOrg, configGithubRepo, configGithubPath, &github.RepositoryContentGetOptions{Ref: "main"})
	if err != nil {
		return nil, err
	}
	if ok, err := client.IsStatusOK(resp); !ok {
		return nil, err
	}
	file, err := configFileContent.GetContent()
	if err != nil {
		return nil, err
	}
	return []byte(file), nil
}

// getTemplateFromGithub downloads template from GitHub.
// It uses default location in test-infra repository.
// Downloaded template is cached to avoid hitting GitHub API rate limits.
func getTemplateFromGithub(ghClient *github.Client, templateFileName string) (string, error) {
	ctx := context.Background()
	templateFilePath := path.Join(templatesDirGithubPath, templateFileName)
	configFileContent, _, resp, err := ghClient.Repositories.GetContents(ctx, configGithubOrg, configGithubRepo, templateFilePath, &github.RepositoryContentGetOptions{Ref: "main"})
	if err != nil {
		return "", err
	}
	if ok, err := client.IsStatusOK(resp); !ok {
		return "", err
	}
	file, err := configFileContent.GetContent()
	if err != nil {
		return "", err
	}
	return file, nil
}

// getFileWalkFunc returns walk function that will recursively find YAML files and will return list of path to these files
func getFileWalkFunc(dataFilesDir string, dataFiles *[]string) filepath.WalkFunc {
	return func(path string, info fs.FileInfo, err error) error {
		// pass the error further, this shouldn't ever happen
		if err != nil {
			return err
		}

		// skip directory entries, we just want files
		if info.IsDir() {
			return nil
		}

		// we only want to check .yaml files
		if !strings.Contains(info.Name(), ".yaml") {
			return nil
		}

		// get relative path
		// dataFile := strings.Replace(path, dataFilesDir, "", -1)
		// add all YAML files to the list
		*dataFiles = append(*dataFiles, path)

		return nil
	}
}

// renderTemplate loads the template and calls the function that renders final files
func renderTemplate(dataFilesDir string, templateConfig *rt.TemplateConfig, config *rt.Config, tplCache map[string]*template.Template) error {
	for _, fromTo := range templateConfig.FromTo {
		var (
			templateInstance *template.Template
			err              error
		)
		if *showOutputDir {
			log.Printf("Rendering %s", fromTo)
		}
		if *templatesDirPath != "" {
			templatePath := path.Join(*templatesDirPath, fromTo.From)
			templateInstance, err = loadTemplate(templatePath, tplCache)
		} else {
			templateInstance, err = loadTemplateFromGithub(fromTo.From, tplCache)
		}
		if err != nil {
			return err
		}
		for _, render := range templateConfig.RenderConfigs {
			err = renderFileFromTemplate(dataFilesDir, templateInstance, *render, config, fromTo)
			if err != nil {
				log.Printf("Failed render %s file", fromTo.To)
				return err
			}
		}
	}

	return nil
}

// renderFileFromTemplate renders template to file, based on the data passed to the template
func renderFileFromTemplate(basePath string, templateInstance *template.Template, renderConfig rt.RenderConfig, config *rt.Config, fromTo rt.FromTo) error {
	relativeDestPath := path.Join(basePath, fromTo.To)

	destDir := path.Dir(relativeDestPath)
	err := os.MkdirAll(destDir, os.ModePerm)
	if err != nil {
		return err
	}

	destFile, err := os.Create(relativeDestPath)
	if err != nil {
		return err
	}

	if err := addAutogeneratedHeader(destFile); err != nil {
		return err
	}

	values := map[string]interface{}{"Values": renderConfig.Values, "Global": config.Global}

	return templateInstance.Execute(destFile, values)
}

// loadTemplate load template read from local file path.
func loadTemplate(templatePath string, tplCache map[string]*template.Template) (*template.Template, error) {
	templateInstance := getTemplateFromCache(templatePath, tplCache)
	if templateInstance != nil {
		return templateInstance, nil
	}
	templateInstance, err = template.
		New(path.Base(templatePath)).
		Funcs(sprig.TxtFuncMap()).
		Funcs(additionalFuncs).
		ParseFiles(templatePath)
	if err != nil {
		return nil, err
	}
	addTemplateToCache(templatePath, templateInstance, tplCache)
	return templateInstance, nil
}

// loadTemplateFromGithub load template downloaded from GitHub.
func loadTemplateFromGithub(templateFileName string, tplCache map[string]*template.Template) (*template.Template, error) {
	templateInstance := getTemplateFromCache(templateFileName, tplCache)
	if templateInstance != nil {
		return templateInstance, nil
	}
	templateString, err := getTemplateFromGithub(ghClient, templateFileName)
	if err != nil {
		return nil, err
	}
	templateInstance, err = template.
		New(path.Base(templateFileName)).
		Funcs(sprig.TxtFuncMap()).
		Funcs(additionalFuncs).
		Parse(templateString)
	if err != nil {
		return nil, err
	}
	addTemplateToCache(templateFileName, templateInstance, tplCache)
	return templateInstance, nil
}

// getTemplateFromCache will return a template from local cache. A template lookup is based on provided cacheKey.
func getTemplateFromCache(cacheKey string, cache map[string]*template.Template) *template.Template {
	if tpl, ok := cache[cacheKey]; ok {
		return tpl
	}
	return nil
}

// addTemplateToCache will add template to the cache. A cacheKey will be used as a map key for template entry.
// This key is used for template lookup when searchin for template in cache.
func addTemplateToCache(cacheKey string, tpl *template.Template, cache map[string]*template.Template) {
	cache[cacheKey] = tpl
}

func addAutogeneratedHeader(destFile *os.File) error {
	outputExt := filepath.Ext(destFile.Name())
	sign, err := commentSign(outputExt)
	if err != nil {
		return err
	}

	header := fmt.Sprintf("%s %s\n\n", sign, autogenerationMessage)
	if _, err := destFile.WriteString(header); err != nil {
		return err
	}

	return nil
}

func commentSign(extension string) (string, error) {
	for sign, extFile := range commentSignByFileExt {
		if extFile.Has(extension) {
			return sign, nil
		}
	}
	return "", fmt.Errorf("cannot add autogenerated header comment: unknow comment sign for %q file extension", extension)
}

// hasProwjobType check if prowjobtype value is present in prowjob configs.
func hasProwjobType(r []rt.Repo, prowjobtype string) bool {
	for _, repo := range r {
		for _, job := range repo.Jobs {
			if _, ok := job.JobConfig[prowjobtype]; ok {
				return ok
			}
		}
	}
	return false
}

// hasPresubmit check if any prowjob is type_presubmit
func hasPresubmit(r []rt.Repo) bool {
	return hasProwjobType(r, "type_presubmit")
}

// hasPresubmit check if any prowjob is type_postsubmit
func hasPostsubmit(r []rt.Repo) bool {
	return hasProwjobType(r, "type_postsubmit")
}

// hasPresubmit check if any prowjob is type_periodic
func hasPeriodic(r []rt.Repo) bool {
	return hasProwjobType(r, "type_periodic")
}

// getRunID trims the prowjob name to 63 characters and makes sure it doesn't end with dash to match pubsub requirements.
func getRunID(name interface{}) string {
	jobName := name.(string)
	if len(jobName) > 63 {
		jobName = jobName[0:63]
		for jobName[len(jobName)-1:] == "-" {
			jobName = jobName[:len(jobName)-1]
		}
	}
	return "\"" + jobName + "\""
}
