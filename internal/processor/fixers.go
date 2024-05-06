package processor

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/trustwallet/assets-go-libs/file"
	"github.com/trustwallet/assets-go-libs/image"
	"github.com/trustwallet/assets-go-libs/path"
	"github.com/trustwallet/assets-go-libs/validation"
	"github.com/trustwallet/assets-go-libs/validation/info"
	"github.com/trustwallet/go-primitives/address"
	"github.com/trustwallet/go-primitives/coin"
	"github.com/trustwallet/go-primitives/types"

	log "github.com/sirupsen/logrus"
)

func (s *Service) FixJSON(f *file.AssetFile) error {
	return file.FormatJSONFile(f.Path())
}

func (s *Service) FixETHAddressChecksum(f *file.AssetFile) error {
	if !coin.IsEVM(f.Chain().ID) {
		return nil
	}

	assetDir := filepath.Base(f.Path())

	err := validation.ValidateETHForkAddress(f.Chain(), assetDir)
	if err != nil {
		checksum, e := address.EIP55Checksum(assetDir)
		if e != nil {
			return fmt.Errorf("failed to get checksum: %s", e)
		}

		newName := path.GetAssetPath(f.Chain().Handle, checksum)

		if e = os.Rename(f.Path(), newName); e != nil {
			return fmt.Errorf("failed to rename dir: %s", e)
		}

		s.fileService.UpdateFile(f, checksum)

		log.WithField("from", assetDir).
			WithField("to", checksum).
			Debug("Renamed asset")
	}

	return nil
}

func (s *Service) FixLogo(f *file.AssetFile) error {
	width, height, err := image.GetPNGImageDimensions(f.Path())
	if err != nil {
		return err
	}

	var isLogoTooLarge bool
	if width > MedW || height > MedH {
		isLogoTooLarge = true
	}

	if isLogoTooLarge {
		log.WithField("path", f.Path()).Debug("Fixing too large image")

		targetW, targetH := calculateTargetDimension(width, height)

		err = image.ResizePNG(f.Path(), targetW, targetH)
		if err != nil {
			return err
		}
	}

	err = validation.ValidateLogoFileSize(f.Path())
	if err != nil { //nolint:static-check
		// TODO: Compress images.
		log.WithField("path", f.Path()).Debug("Fixing logo file size image")
		err = ReFileSizePNG(f.Path())
		if err != nil {
			return err
		}
	}

	return nil
}

const (
	bytesInKB   = 1024
	sizeLimitKB = 100

	MaxW = 512
	MaxH = 512
	MedW = 256
	MedH = 256
	MinW = 128
	MinH = 128
)

func ReFileSizePNG(path string) error {
	targetSize := sizeLimitKB * bytesInKB
	inputPath := path
	outputPath := path
	// Open the source file
	file, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Decode the image
	src, err := png.Decode(file)
	if err != nil {
		return fmt.Errorf("failed to decode image: %w", err)
	}

	// Create a buffer to store the encoded image
	var buf bytes.Buffer
	// Encode the image to PNG format with the best quality
	err = png.Encode(&buf, src)
	if err != nil {
		return fmt.Errorf("failed to encode image: %w", err)
	}

	// If the image size already meets the target size, directly write the buffer content to the output file
	if buf.Len() <= targetSize {
		err = writeToFile(outputPath, buf.Bytes())
		if err != nil {
			return err
		}
		return nil
	}

	pngEncoder := png.Encoder{}
	var compressedBuf bytes.Buffer

	// If the image size exceeds the target size, try different compression qualities until the target size is met
	for quality := png.BestCompression; quality >= png.NoCompression; quality-- {
		pngEncoder.CompressionLevel = quality

		compressedBuf.Reset()
		// Create a new PNG encoder with the specified compression quality
		err = pngEncoder.Encode(&compressedBuf, src)
		if err != nil {
			return fmt.Errorf("failed to encode image: %w", err)
		}
		// Check if the size of the compressed image meets the target size
		if compressedBuf.Len() <= targetSize {
			err = writeToFile(outputPath, compressedBuf.Bytes())
			if err != nil {
				return err
			}
			return nil
		}
	}

	return fmt.Errorf("unable to compress the image to the target size")
}

// writeToFile writes data to a file.
func writeToFile(path string, data []byte) error {
	output, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer output.Close()

	_, err = output.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write to file: %w", err)
	}
	return nil
}

func calculateTargetDimension(width, height int) (targetW, targetH int) {
	widthFloat := float32(width)
	heightFloat := float32(height)

	maxEdge := widthFloat
	if heightFloat > widthFloat {
		maxEdge = heightFloat
	}

	ratio := MedW / maxEdge

	targetW = int(widthFloat * ratio)
	targetH = int(heightFloat * ratio)

	return targetW, targetH
}

func (s *Service) FixChainInfoJSON(f *file.AssetFile) error {
	var chainInfo info.CoinModel

	err := file.ReadJSONFile(f.Path(), &chainInfo)
	if err != nil {
		return err
	}

	expectedType := string(types.Coin)
	if chainInfo.Type == nil || *chainInfo.Type != expectedType {
		chainInfo.Type = &expectedType

		data, err := file.PrepareJSONData(&chainInfo)
		if err != nil {
			return err
		}

		return file.CreateJSONFile(f.Path(), data)
	}

	return nil
}

func (s *Service) FixAssetInfo(f *file.AssetFile) error {
	var assetInfo info.AssetModel

	err := file.ReadJSONFile(f.Path(), &assetInfo)
	if err != nil {
		return err
	}

	var isModified bool

	// Fix asset type.
	var assetType string
	if assetInfo.Type != nil {
		assetType = *assetInfo.Type
	}

	// We need to skip error check to fix asset type if it's incorrect or empty.
	chain, _ := types.GetChainFromAssetType(assetType)

	expectedTokenType, ok := types.GetTokenType(f.Chain().ID, f.Asset())
	if !ok {
		expectedTokenType = strings.ToUpper(assetType)
	}

	// https://github.com/trustwallet/backend/issues/2561
	if chain.ID != coin.CRYPTOORG && chain.ID != coin.CRONOS {
		if chain.ID != f.Chain().ID || !strings.EqualFold(assetType, expectedTokenType) {
			assetInfo.Type = &expectedTokenType
			isModified = true
		}
	}

	// Fix asset id.
	assetID := f.Asset()
	if assetInfo.ID == nil || *assetInfo.ID != assetID {
		assetInfo.ID = &assetID
		isModified = true
	}

	expectedExplorerURL, err := coin.GetCoinExploreURL(f.Chain(), f.Asset(), assetType)
	if err != nil {
		return err
	}

	// Fix asset explorer url.
	if assetInfo.Explorer == nil || !strings.EqualFold(expectedExplorerURL, *assetInfo.Explorer) {
		assetInfo.Explorer = &expectedExplorerURL
		isModified = true
	}

	if isModified {
		data, err := file.PrepareJSONData(&assetInfo)
		if err != nil {
			return err
		}

		return file.CreateJSONFile(f.Path(), data)
	}

	return nil
}
