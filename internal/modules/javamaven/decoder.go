// SPDX-License-Identifier: Apache-2.0

package javamaven

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"spdx-sbom-generator/internal/helper"
	"spdx-sbom-generator/internal/models"
	"strings"
)

// Update package supplier information
func updatePackageSuppier(mod models.Module, developer Developer) {
	if len(developer.Name) > 0 && len(developer.Email) > 0 {
		mod.Supplier.Type = "Person"
		mod.Supplier.Name = developer.Name
		mod.Supplier.Email = developer.Email
	} else if len(developer.Email) == 0 && len(developer.Name) > 0 {
		mod.Supplier.Type = "Person"
		mod.Supplier.Name = developer.Name
	}

	// check for organization tag
	if len(developer.Organization) > 0 {
		mod.Supplier.Type = "Organization"
	}
}

// Update package download location
func updatePackageDownloadLocation(mod models.Module, distManagement DistributionManagement) {
	if len(distManagement.DownloadUrl) > 0 && (strings.HasPrefix(distManagement.DownloadUrl, "http") ||
		strings.HasPrefix(distManagement.DownloadUrl, "https")) {
		// ******** TODO Module has only PackageHomePage, it does not have PackageDownloadLocation field
		//mod.PackageDownloadLocation = distManagement.DownloadUrl
	}
}

// captures os.Stdout data and writes buffers
func stdOutCapture() func() (string, error) {
	readFromPipe, writeToPipe, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	done := make(chan error, 1)

	save := os.Stdout
	os.Stdout = writeToPipe

	var buffer strings.Builder

	go func() {
		_, err := io.Copy(&buffer, readFromPipe)
		readFromPipe.Close()
		done <- err
	}()

	return func() (string, error) {
		os.Stdout = save
		writeToPipe.Close()
		err := <-done
		return buffer.String(), err
	}
}

func getDependencyList() ([]string, error) {
	done := stdOutCapture()

	// TODO add error handling
	cmd1 := exec.Command("mvn", "-o", "dependency:list")
	cmd2 := exec.Command("grep", ":.*:.*:.*")
	cmd3 := exec.Command("cut", "-d]", "-f2-")
	cmd4 := exec.Command("sort", "-u")
	cmd2.Stdin, _ = cmd1.StdoutPipe()
	cmd3.Stdin, _ = cmd2.StdoutPipe()
	cmd4.Stdin, _ = cmd3.StdoutPipe()
	cmd4.Stdout = os.Stdout
	_ = cmd4.Start()
	_ = cmd3.Start()
	_ = cmd2.Start()
	_ = cmd1.Run()
	_ = cmd2.Wait()
	_ = cmd3.Wait()
	_ = cmd4.Wait()

	capturedOutput, err := done()
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	s := strings.Split(capturedOutput, "\n")
	return s, err
}

func convertMavenPackageToModule(project MavenPomProject) models.Module {
	// package to module
	var mod models.Module
	if len(project.Name) == 0 {
		mod.Name = strings.Replace(project.ArtifactId, " ", "-", -1)
	} else {
		mod.Name = strings.Replace(project.Name, " ", "-", -1)
	}
	mod.Version = project.Version
	mod.Root = true
	updatePackageSuppier(mod, project.Developers)
	updatePackageDownloadLocation(mod, project.DistributionManagement)
	if len(project.Url) > 0 {
		mod.PackageHomePage = project.Url
	}
	mod.Modules = map[string]*models.Module{}
	mod.CheckSum = &models.CheckSum{
		Algorithm: models.HashAlgoSHA1,
		Value:     readCheckSum(mod.Path),
	}

	licensePkg, err := helper.GetLicenses(".")
	if err == nil {
		mod.LicenseDeclared = helper.BuildLicenseDeclared(licensePkg.ID)
		mod.LicenseConcluded = helper.BuildLicenseConcluded(licensePkg.ID)
		mod.Copyright = helper.GetCopyright(licensePkg.ExtractedText)
		mod.CommentsLicense = licensePkg.Comments
	}

	return mod
}

func convertPOMReaderToModules(fpath string) ([]models.Module, error) {
	modules := make([]models.Module, 0)

	filePath := fpath + "/pom.xml"
	pomFile, err := os.Open(filePath)
	if err != nil {
		fmt.Println(err)
		return modules, err
	}
	defer pomFile.Close()

	// read our opened xmlFile as a byte array.
	pomStr, _ := ioutil.ReadAll(pomFile)

	// Load project from string
	var project MavenPomProject
	if err := xml.Unmarshal([]byte(pomStr), &project); err != nil {
		fmt.Println("unable to unmarshal pom file. Reason: %s", err)
		return modules, err
	}

	dependencyList, err := getDependencyList()
	if err != nil {
		fmt.Println("error in getting mvn dependency list and parsing it")
		return modules, err
	}

	mod := convertMavenPackageToModule(project)
	modules = append(modules, mod)

	// iterate over Modules
	for _, module := range project.Modules {
		var mod models.Module
		mod.Name = module
		mod.Modules = map[string]*models.Module{}
		// set package version as module version
		mod.Version = project.Version
		mod.CheckSum = &models.CheckSum{
			Algorithm: models.HashAlgoSHA1,
			Value:     readCheckSum(module),
		}
		modules = append(modules, mod)
	}

	// iterate over dependencyManagement
	for _, dependencyManagement := range project.DependencyManagement.Dependencies {
		var mod models.Module
		mod.Name = path.Base(dependencyManagement.ArtifactId)
		if len(project.Properties) > 0 {
			version := strings.TrimLeft(strings.TrimRight(dependencyManagement.Version, "}"), "${")
			mod.Version = project.Properties[version]
		}
		mod.Modules = map[string]*models.Module{}
		mod.CheckSum = &models.CheckSum{
			Algorithm: models.HashAlgoSHA1,
			Value:     readCheckSum(dependencyManagement.ArtifactId),
		}
		modules = append(modules, mod)
	}

	// iterate over dependencies
	for _, dep := range project.Dependencies {
		var mod models.Module
		mod.Name = path.Base(dep.ArtifactId)
		mod.Version = dep.Version
		mod.Modules = map[string]*models.Module{}
		mod.CheckSum = &models.CheckSum{
			Algorithm: models.HashAlgoSHA1,
			Value:     readCheckSum(dep.ArtifactId),
		}
		modules = append(modules, mod)
	}

	// Add additional dependency from mvn dependency list to pom.xml dependency list
	var i int
	for i < len(dependencyList)-2 { // skip 1 empty line and Finished statement line
		dependencyItem := strings.Split(dependencyList[i], ":")[1]

		found := false
		// iterate over dependencies
		for _, dep := range project.Dependencies {
			if dep.ArtifactId == dependencyItem {
				found = true
				break
			}
		}

		if !found {
			for _, dependencyManagement := range project.DependencyManagement.Dependencies {
				if dependencyManagement.ArtifactId == dependencyItem {
					found = true
					break
				}
			}
		}

		if !found {
			var mod models.Module
			mod.Name = path.Base(dependencyItem)
			mod.Version = strings.Split(dependencyList[i], ":")[3]
			mod.Modules = map[string]*models.Module{}
			mod.CheckSum = &models.CheckSum{
				Algorithm: models.HashAlgoSHA1,
				Value:     readCheckSum(dependencyItem),
			}
			modules = append(modules, mod)
		}
		i++
	}

	// iterate over Plugins
	for _, plugin := range project.Build.Plugins {
		var mod models.Module
		mod.Name = path.Base(plugin.ArtifactId)
		mod.Version = plugin.Version
		mod.Modules = map[string]*models.Module{}
		mod.CheckSum = &models.CheckSum{
			Algorithm: models.HashAlgoSHA1,
			Value:     readCheckSum(plugin.ArtifactId),
		}
		modules = append(modules, mod)
	}

	return modules, nil
}

func getTransitiveDependencyList() (map[string][]string, error) {
	path := "/tmp/JavaMavenTDTreeOutput.txt"
	os.Remove(path)

	command := exec.Command("mvn", "dependency:tree", "-DappendOutput=true", "-DoutputFile=/tmp/JavaMavenTDTreeOutput.txt")
	_, err := command.Output()
	if err != nil {
		return nil, err
	}

	tdList, err1 := readAndgetTransitiveDependencyList()
	if err1 != nil {
		return nil, err1
	}
	return tdList, nil
}

func readAndgetTransitiveDependencyList() (map[string][]string, error) {

	file, err := os.Open("/tmp/JavaMavenTDTreeOutput.txt")

	if err != nil {
		log.Println(err)
		return nil, err
	}

	scanner := bufio.NewScanner(file)

	scanner.Split(bufio.ScanLines)
	var text []string

	for scanner.Scan() {
		text = append(text, scanner.Text())
	}
	file.Close()

	tdList := map[string][]string{}
	handlePkgs(text, tdList)
	return tdList, nil
}

func isSubPackage(name string) (int, bool) {
	if strings.HasPrefix(name, "\\-") || strings.HasPrefix(name, "+-") {
		return 1, true
	} else if strings.Contains(name, "   \\-") || strings.Contains(name, "|  \\- ") {
		return 2, true
	}
	return 0, false
}

func handlePkgs(text []string, tdList map[string][]string) {
	i := 0
	var pkgName, subpkg, currentTextVal string
	subPkgs := make([]string, 0)

	for i < len(text) {
		level, isTrue := isSubPackage(text[i])

		if !isTrue {
			pkgName = strings.Split(text[i], ":")[1]
			subPkgs = nil
		} else {
			subpkg = strings.Split(text[i], ":")[1]
			if level == 1 {
				subPkgs = append(subPkgs, subpkg)
				tdList[pkgName] = subPkgs
			} else if level == 2 {
				tdList[currentTextVal] = []string{subpkg}
			}
		}
		// store previous line item
		currentTextVal = subpkg
		i++
	}
}

func buildDependenciesGraph(modules []models.Module, tdList map[string][]string) error {
	moduleMap := map[string]models.Module{}
	moduleIndex := map[string]int{}

	for idx, module := range modules {
		moduleMap[module.Name] = module
		moduleIndex[module.Name] = idx
	}

	for i := range tdList {
		for j := range tdList[i] {

			if len(tdList[i][j]) > 0 {
				moduleName := i
				if _, ok := moduleMap[moduleName]; !ok {
					continue
				}

				depName := tdList[i][j]
				depModule, ok := moduleMap[depName]
				if !ok {
					continue
				}

				modules[moduleIndex[moduleName]].Modules[depName] = &models.Module{
					Name:             depModule.Name,
					Version:          depModule.Version,
					Path:             depModule.Path,
					LocalPath:        depModule.LocalPath,
					Supplier:         depModule.Supplier,
					PackageURL:       depModule.PackageURL,
					CheckSum:         depModule.CheckSum,
					PackageHomePage:  depModule.PackageHomePage,
					LicenseConcluded: depModule.LicenseConcluded,
					LicenseDeclared:  depModule.LicenseDeclared,
					CommentsLicense:  depModule.CommentsLicense,
					OtherLicense:     depModule.OtherLicense,
					Copyright:        depModule.Copyright,
					PackageComment:   depModule.PackageComment,
					Root:             depModule.Root,
				}
			}
		}
	}

	return nil
}