// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type Config

package googlecomputeimport

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/storage/v1"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-googlecompute/lib/common"
	sdk_common "github.com/hashicorp/packer-plugin-sdk/common"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

type Config struct {
	sdk_common.PackerConfig `mapstructure:",squash"`

	// Authentication methods

	// A temporary [OAuth 2.0 access token](https://developers.google.com/identity/protocols/oauth2)
	// obtained from the Google Authorization server, i.e. the `Authorization: Bearer` token used to
	// authenticate HTTP requests to GCP APIs.
	// This is an alternative to `account_file`, and ignores the `scopes` field.
	// If both are specified, `access_token` will be used over the `account_file` field.
	//
	// These access tokens cannot be renewed by Packer and thus will only work until they expire.
	// If you anticipate Packer needing access for longer than a token's lifetime (default `1 hour`),
	// please use a service account key with `account_file` instead.
	AccessToken string `mapstructure:"access_token" required:"false"`
	// The JSON file containing your account credentials. Not required if you
	// run Packer on a GCE instance with a service account. Instructions for
	// creating the file or using service accounts are above.
	AccountFile string `mapstructure:"account_file" required:"false"`
	// The JSON file containing your account credentials.
	//
	// The file's contents may be anything supported by the Google Go client, i.e.:
	//
	// * Service account JSON
	// * OIDC-provided token for federation
	// * Gcloud user credentials file (refresh-token JSON)
	// * A Google Developers Console client_credentials.json
	CredentialsFile string `mapstructure:"credentials_file" required:"false"`
	// The raw JSON payload for credentials.
	//
	// The accepted data formats are same as those described under
	// [credentials_file](#credentials_file).
	CredentialsJSON string `mapstructure:"credentials_json" required:"false"`
	// This allows service account impersonation as per the [docs](https://cloud.google.com/iam/docs/impersonating-service-accounts).
	ImpersonateServiceAccount string `mapstructure:"impersonate_service_account" required:"false"`
	// Can be set instead of account_file. If set, this builder will use
	// HashiCorp Vault to generate an Oauth token for authenticating against
	// Google Cloud. The value should be the path of the token generator
	// within vault.
	// For information on how to configure your Vault + GCP engine to produce
	// Oauth tokens, see https://www.vaultproject.io/docs/auth/gcp
	// You must have the environment variables VAULT_ADDR and VAULT_TOKEN set,
	// along with any other relevant variables for accessing your vault
	// instance. For more information, see the Vault docs:
	// https://www.vaultproject.io/docs/commands/#environment-variables
	// Example:`"vault_gcp_oauth_engine": "gcp/token/my-project-editor",`
	VaultGCPOauthEngine string `mapstructure:"vault_gcp_oauth_engine"`
	credentials         *google.Credentials

	// The service account scopes for launched importer post-processor instance.
	// Defaults to:
	//
	// ```json
	// [
	//   "https://www.googleapis.com/auth/cloud-platform"
	// ]
	// ```
	Scopes []string `mapstructure:"scopes" required:"false"`
	//The project ID where the GCS bucket exists and where the GCE image is stored.
	ProjectId string `mapstructure:"project_id" required:"true"`
	IAP       bool   `mapstructure-to-hcl:",skip"`
	//The name of the GCS bucket where the raw disk image will be uploaded.
	Bucket string `mapstructure:"bucket" required:"true"`
	//The name of the GCS object in `bucket` where
	//the RAW disk image will be copied for import. This is treated as a
	//[template engine](/packer/docs/templates/legacy_json_templates/engine). Therefore, you
	//may use user variables and template functions in this field. Defaults to
	//`packer-import-{{timestamp}}.tar.gz`.
	GCSObjectName string `mapstructure:"gcs_object_name"`
	// Specifies the architecture or processor type that this image can support. Must be one of: `arm64` or `x86_64`. Defaults to `ARCHITECTURE_UNSPECIFIED`.
	ImageArchitecture string `mapstructure:"image_architecture"`
	//The description of the resulting image.
	ImageDescription string `mapstructure:"image_description"`
	//The name of the image family to which the resulting image belongs.
	ImageFamily string `mapstructure:"image_family"`
	//A list of features to enable on the guest operating system. Applicable only for bootable images. Valid
	//values are `MULTI_IP_SUBNET`, `UEFI_COMPATIBLE`,
	//`VIRTIO_SCSI_MULTIQUEUE`, `GVNIC` and `WINDOWS` currently.
	ImageGuestOsFeatures []string `mapstructure:"image_guest_os_features"`
	//Key/value pair labels to apply to the created image.
	ImageLabels map[string]string `mapstructure:"image_labels"`
	//The unique name of the resulting image.
	ImageName string `mapstructure:"image_name" required:"true"`
	//Specifies a Cloud Storage location, either regional or multi-regional, where image content is to be stored. If not specified, the multi-region location closest to the source is chosen automatically.
	ImageStorageLocations []string `mapstructure:"image_storage_locations"`
	//Skip removing the TAR file uploaded to the GCS
	//bucket after the import process has completed. "true" means that we should
	//leave it in the GCS bucket, "false" means to clean it out. Defaults to
	//`false`.
	SkipClean bool `mapstructure:"skip_clean"`
	//A key used to establish the trust relationship between the platform owner and the firmware. You may only specify one platform key, and it must be a valid X.509 certificate.
	ImagePlatformKey string `mapstructure:"image_platform_key"`
	//A key used to establish a trust relationship between the firmware and the OS. You may specify multiple comma-separated keys for this value.
	ImageKeyExchangeKey []string `mapstructure:"image_key_exchange_key"`
	//A database of certificates that are trusted and can be used to sign boot files. You may specify single or multiple comma-separated values for this value.
	ImageSignaturesDB []string `mapstructure:"image_signatures_db"`
	//A database of certificates that have been revoked and will cause the system to stop booting if a boot file is signed with one of them. You may specify single or multiple comma-separated values for this value.
	ImageForbiddenSignaturesDB []string `mapstructure:"image_forbidden_signatures_db"`

	ctx interpolate.Context
}

type PostProcessor struct {
	config Config
}

func (p *PostProcessor) CheckAuth() error {
	return googlecompute.CheckAuth(
		p.config.AccessToken,
		p.config.AccountFile,
		p.config.CredentialsFile,
		p.config.CredentialsJSON,
		p.config.ImpersonateServiceAccount,
		p.config.VaultGCPOauthEngine,
	)
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec { return p.config.FlatMapstructure().HCL2Spec() }

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         BuilderId,
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"gcs_object_name",
			},
		},
	}, raws...)
	if err != nil {
		return err
	}

	errs := new(packersdk.MultiError)

	// Set defaults
	if p.config.GCSObjectName == "" {
		p.config.GCSObjectName = "packer-import-{{timestamp}}.tar.gz"
	}

	// Check and render gcs_object_name
	if err = interpolate.Validate(p.config.GCSObjectName, &p.config.ctx); err != nil {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("Error parsing gcs_object_name template: %s", err))
	}

	if p.config.ImageArchitecture == "" {
		// Lower case is not required here
		p.config.ImageArchitecture = "ARCHITECTURE_UNSPECIFIED"
	} else {
		// The api is unclear on what case is expected for here and inconsistent across https://cloud.google.com/compute/docs/reference/rest/v1/machineImages
		// vs https://cloud.google.com/compute/docs/images/create-custom#guest-os-features but lower case works
		p.config.ImageArchitecture = strings.ToLower(p.config.ImageArchitecture)
		if p.config.ImageArchitecture != "x86_64" && p.config.ImageArchitecture != "arm64" {
			errs = packersdk.MultiErrorAppend(errs,
				fmt.Errorf("Invalid image architecture: Must be one of x86_64 or arm64"))
		}
	}

	err = p.CheckAuth()
	if err != nil {
		errs = packersdk.MultiErrorAppend(errs, err)
	}

	// Authenticating via an account file
	if p.config.AccountFile != "" {
		log.Printf("account_file is deprecated, please use either credentials_json or credentials_file instead")
		// Heuristic, but should be good enough to discriminate between
		// the two somewhat reliably.
		if strings.HasPrefix(strings.TrimSpace(p.config.AccountFile), "{") {
			p.config.CredentialsJSON = p.config.AccountFile
		} else {
			p.config.CredentialsFile = p.config.AccountFile
		}
	}

	if p.config.CredentialsFile != "" {
		cfg, err := googlecompute.ProcessCredentialsFile(p.config.CredentialsFile)
		if err != nil {
			errs = packersdk.MultiErrorAppend(errs, err)
		}
		p.config.credentials = cfg
	}

	if p.config.CredentialsJSON != "" {
		cfg, err := googlecompute.ProcessCredentials([]byte(p.config.CredentialsJSON))
		if err != nil {
			errs = packersdk.MultiErrorAppend(errs, err)
		}
		p.config.credentials = cfg
	}

	if len(p.config.Scopes) == 0 {
		p.config.Scopes = []string{
			storage.CloudPlatformScope,
		}
	}

	templates := map[string]*string{
		"bucket":     &p.config.Bucket,
		"image_name": &p.config.ImageName,
		"project_id": &p.config.ProjectId,
	}
	for key, ptr := range templates {
		if *ptr == "" {
			errs = packersdk.MultiErrorAppend(
				errs, fmt.Errorf("%s must be set", key))
		}
	}

	if len(errs.Errors) > 0 {
		return errs
	}

	return nil
}

func (p *PostProcessor) PostProcess(ctx context.Context, ui packersdk.Ui, artifact packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	generatedData := artifact.State("generated_data")
	if generatedData == nil {
		// Make sure it's not a nil map so we can assign to it later.
		generatedData = make(map[string]interface{})
	}
	p.config.ctx.Data = generatedData
	var err error

	cfg := &common.GCEDriverConfig{
		Ui:     ui,
		Scopes: p.config.Scopes,
	}
	p.config.Authentication.ApplyDriverConfig(cfg)
	driver, err := common.NewDriverGCE(*cfg)
	if err != nil {
		return nil, false, false, err
	}

	switch artifact.BuilderId() {
	// TODO: uncomment when Packer core stops importing this plugin.
	// case compress.BuilderId, artifice.BuilderId:
	case "packer.post-processor.compress", "packer.post-processor.artifice":
		break
	default:
		err := fmt.Errorf(
			"Unknown artifact type: %s\nCan only import from Compress post-processor and Artifice post-processor artifacts.",
			artifact.BuilderId())
		return nil, false, false, err
	}

	p.config.GCSObjectName, err = interpolate.Render(p.config.GCSObjectName, &p.config.ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("Error rendering gcs_object_name template: %s", err)
	}

	tarball, err := p.findTarballFromArtifact(artifact)

	rawImageGcsPath, err := driver.UploadToBucket(p.config.Bucket, p.config.GCSObjectName, tarball)
	if err != nil {
		return nil, false, false, err
	}

	shieldedVMStateConfig, err := CreateShieldedVMStateConfig(p.config.ImageGuestOsFeatures, p.config.ImagePlatformKey, p.config.ImageKeyExchangeKey, p.config.ImageSignaturesDB, p.config.ImageForbiddenSignaturesDB)
	if err != nil {
		return nil, false, false, err
	}

	var retArtifact *Artifact
	var retErr error

	imageCh, errCh := driver.CreateImageFromRaw(p.config.ProjectId, rawImageGcsPath, p.config.ImageName, p.config.ImageDescription, p.config.ImageFamily, p.config.ImageLabels, p.config.ImageGuestOsFeatures, shieldedVMStateConfig, p.config.ImageStorageLocations, p.config.ImageArchitecture)
	select {
	case img := <-imageCh:
		retArtifact = &Artifact{
			paths: []string{
				img.SelfLink,
			},
		}
	case err := <-errCh:
		retErr = err
	}

	if err != nil {
		ui.Say(fmt.Sprintf("failed to create image from raw disk: %s", err))
	}

	if !p.config.SkipClean {
		ui.Say(fmt.Sprintf("deleting %s from bucket %s", p.config.GCSObjectName, p.config.Bucket))
		err = driver.DeleteFromBucket(p.config.Bucket, p.config.GCSObjectName)
		if err != nil {
			return nil, false, false, err
		}
	}

	return retArtifact, false, false, retErr
}

func (p PostProcessor) findTarballFromArtifact(artifact packersdk.Artifact) (io.Reader, error) {
	source := ""
	for _, path := range artifact.Files() {
		if strings.HasSuffix(path, ".tar.gz") {
			source = path
			break
		}
	}

	if source == "" {
		return nil, fmt.Errorf("No tar.gz file found in list of artifacts")
	}

	return os.Open(source)
}

func FillFileContentBuffer(certOrKeyFile string) (*compute.FileContentBuffer, error) {
	data, err := ioutil.ReadFile(certOrKeyFile)
	if err != nil {
		err := fmt.Errorf("Unable to read Certificate or Key file %s", certOrKeyFile)
		return nil, err
	}
	shield := &compute.FileContentBuffer{
		Content:  base64.StdEncoding.EncodeToString(data),
		FileType: "X509",
	}
	block, _ := pem.Decode(data)

	if block == nil || block.Type != "CERTIFICATE" {
		_, err = x509.ParseCertificate(data)
	} else {
		_, err = x509.ParseCertificate(block.Bytes)
	}
	if err != nil {
		shield.FileType = "BIN"
	}
	return shield, nil

}

func CreateShieldedVMStateConfig(imageGuestOsFeatures []string, imagePlatformKey string, imageKeyExchangeKey []string, imageSignaturesDB []string, imageForbiddenSignaturesDB []string) (*compute.InitialStateConfig, error) {
	shieldedVMStateConfig := &compute.InitialStateConfig{}
	for _, v := range imageGuestOsFeatures {
		if v == "UEFI_COMPATIBLE" {
			if imagePlatformKey != "" {
				shieldedData, err := FillFileContentBuffer(imagePlatformKey)
				if err != nil {
					return nil, err
				}
				shieldedVMStateConfig.Pk = shieldedData
			}
			for _, v := range imageKeyExchangeKey {
				shieldedData, err := FillFileContentBuffer(v)
				if err != nil {
					return nil, err
				}
				shieldedVMStateConfig.Keks = append(shieldedVMStateConfig.Keks, shieldedData)
			}
			for _, v := range imageSignaturesDB {
				shieldedData, err := FillFileContentBuffer(v)
				if err != nil {
					return nil, err
				}
				shieldedVMStateConfig.Dbs = append(shieldedVMStateConfig.Dbs, shieldedData)
			}
			for _, v := range imageForbiddenSignaturesDB {
				shieldedData, err := FillFileContentBuffer(v)
				if err != nil {
					return nil, err
				}
				shieldedVMStateConfig.Dbxs = append(shieldedVMStateConfig.Dbxs, shieldedData)
			}

		}
	}
	return shieldedVMStateConfig, nil
}
