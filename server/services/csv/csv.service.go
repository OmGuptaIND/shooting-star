package csvService

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/OmGuptaIND/shooting-star/appError"
	"github.com/OmGuptaIND/shooting-star/cloud"
	"github.com/OmGuptaIND/shooting-star/config/logger"
	"github.com/OmGuptaIND/shooting-star/db"
	"github.com/OmGuptaIND/shooting-star/db/models"
	"github.com/OmGuptaIND/shooting-star/provider"
	"github.com/OmGuptaIND/shooting-star/utils"
	"github.com/google/generative-ai-go/genai"
	"go.uber.org/zap"
)

type csvService struct {
	ctx context.Context
	cancelFunc context.CancelFunc

	logger *zap.Logger
}


const (
	// Number of sample rows to store for the SampleData field ***
	sampleDataRowCount = 3

	// Max rows to check for type inference
	typeInferenceRowCount = 100
)

// New creates a new instance of csvService with the provided context.
func New(ctx context.Context) *csvService {
	ctx, cancelFunc := context.WithCancel(ctx)

	return &csvService{
		ctx:    ctx,
		cancelFunc: cancelFunc,
		logger: logger.FromCtx(ctx).With(zap.String("handler", "csvService")),
	}
}

// Close cancels the context of the csvService, allowing for cleanup.
func (c *csvService) Close() {
	c.cancelFunc()
}

// UploadCSV uploads the CSV file to the server and processes it.
func (c *csvService) UploadCSV(workspace *models.Workspace, fileDetails *CSVDetails) (*models.Upload, error) {
	c.logger.Info("Uploading CSV file", zap.String("fileName", fileDetails.FileName))

	awsClient, err := cloud.NewAwsClient(c.ctx)
	if err != nil {
		c.logger.Error("Error creating AWS client", zap.Error(err))
		return nil, appError.New(appError.InternalError, "failed to create AWS client", err)
	}

	uploadOutput, err := awsClient.UploadFile(&fileDetails.FileName, fileDetails.FilePath)
	if err != nil {
		c.logger.Error("Error uploading file to AWS", zap.Error(err))
		return nil, appError.New(appError.InternalError, "failed to upload file to AWS", err)
	}

	upload := &models.Upload{
		SourceType: models.UploadTypeCSV,
		SourceIdentifier: fileDetails.FileName,
		FileLocation: uploadOutput.Location,
	}
	
	tx := db.Conn.Begin()
	if tx.Error != nil {
        c.logger.Error("Error starting transaction", zap.Error(tx.Error))
        return nil, appError.New(appError.InternalError, "failed to start database transaction", tx.Error)
    }

	if err := tx.Create(&upload).Error; err != nil {
		c.logger.Error("Error creating upload record", zap.Error(err))
		return nil, appError.New(appError.InternalError, err.Error(), err)
	}

	workspaceUploadLink := &models.WorkspaceUpload{
        WorkspaceID:     workspace.ID,
        UploadID:        upload.ID,
    }

	if err := tx.Create(&workspaceUploadLink).Error; err != nil {
        tx.Rollback()
        c.logger.Error("Error creating workspace-upload link", zap.Error(err))
        return nil, appError.New(appError.InternalError, err.Error(), err)
    }

	if err := tx.Commit().Error; err != nil {
        c.logger.Error("Error committing transaction", zap.Error(err))
        tx.Rollback()
        return nil, appError.New(appError.InternalError, "failed to commit transaction", err)
    }

	c.logger.Info("CSV file uploaded successfully", zap.String("FileName", fileDetails.FileName))
	return upload, nil
} 

// ProcessAndEmbedCSV processes the CSV file and embeds it.
func (c *csvService) ProcessAndEmbedCSV(fileDetails *CSVDetails) (error) {
	c.logger.Info("Processing CSV file", zap.String("FileName", fileDetails.FileName))

	genClient, err := provider.NewGeminiProvider(c.ctx)
	if err != nil {
		return appError.New(appError.InternalError, "failed to create Gemini client", err)
	}
	defer genClient.Close()

	em := genClient.EmbeddingModel(string(provider.Embedding001))

	// Create a new batch for embedding, which will be used to store the content
	b := em.NewBatch()
	embeddedContents := make([]string, 0, len(fileDetails.Headers))
	for _, column := range fileDetails.Headers {
		title := fmt.Sprintf("CSV: %s, Column: %s", fileDetails.FileName, column)

		desc, ok := fileDetails.HeadersDescription[column]
		if !ok {
			desc = "No description available"
		}

		content := fmt.Sprintf("Column: %s\nDescription: %s", column, desc)

		b.AddContentWithTitle(title, genai.Text(content))

		embeddedContents = append(embeddedContents, content)
	}

	res, err := em.BatchEmbedContents(c.ctx, b)
	if err != nil {
		return appError.New(appError.InternalError, err.Error(), err)
	}

	c.logger.Info("CSV file processed and embedding created", zap.String("fileName", fileDetails.FileName))

	vectorEmbeddings := make([]*models.VectorEmbedding, 0, len(res.Embeddings))

	for i, e := range res.Embeddings {
		vectorEmbedding := &models.VectorEmbedding{
			SourceType: models.EmbeddingSourceTypeCSVColumn,
			SourceIdentifier: fileDetails.UploadInfo.ID,
			ColumnOrChunkName: fileDetails.Headers[i],
			OriginalText: embeddedContents[i],
			Embedding: e.Values,
		}

		vectorEmbeddings = append(vectorEmbeddings, vectorEmbedding)
	}

	if err := db.Conn.CreateInBatches(vectorEmbeddings, 10).Error; err != nil {
		return appError.New(appError.InternalError, err.Error(), err)
	}

	c.logger.Info("CSV file processed and stored successfully", zap.String("fileName", fileDetails.FileName))

	return nil
}

// ExtractCSVDetails extracts the details of the CSV file, such as headers, data types, and sample data.
// It also infers the data types of each column and retrieves descriptions for each header.
// Asks the Gemini API for header descriptions based on the CSV file's content.
func (c *csvService) ExtractCSVDetails(filePath string) (*CSVDetails, error) {
	details := &CSVDetails{
		FilePath: filePath,
		HeadersDescription: make(map[string]string),
		SampleData: make(map[string]string),
		DataTypes: make(map[string]string),
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, appError.New(appError.InternalError, "failed to get file info", err)
	}

	details.FileName = fileInfo.Name()
	details.FileSize = fileInfo.Size()

	file, err := os.Open(filePath)
	if err != nil {
		return nil, appError.New(appError.InternalError, "failed to open CSV file", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true // Trim leading spaces from each field, usef

	headers, err := reader.Read()
	if err != nil {
		return nil, appError.New(appError.InternalError, "failed to read CSV headers", err)
	}

	details.Headers = headers
	details.NumCols = len(headers)

	reader.FieldsPerRecord = details.NumCols

	columnSamplesForInference := make([][]string, details.NumCols)
	columnSamplesForDisplay := make([][]string, details.NumCols)
	dataRowCount := 0

	for {
		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			var parseError *csv.ParseError
			if errors.As(err, &parseError) {
				log.Printf("CSV parse error in %s at line %d: %v", filePath, parseError.Line, err)
				return nil, fmt.Errorf("csv parse error in %s: %w", filePath, err)
			}
			return nil, fmt.Errorf("failed to read data row from %s: %w", filePath, err)
		}

		dataRowCount++

		isSampleRow := dataRowCount <= sampleDataRowCount
		isTypeInferenceRow := dataRowCount <= typeInferenceRowCount

		for i, value := range record {
			if isSampleRow {
				columnSamplesForDisplay[i] = append(columnSamplesForDisplay[i], value)
			}

			if isTypeInferenceRow {
				trimmedValue := strings.TrimSpace(value)
				if trimmedValue != "" {
					columnSamplesForInference[i] = append(columnSamplesForInference[i], trimmedValue)
				}
			}
		}

		if isTypeInferenceRow && len(columnSamplesForInference[0]) >= typeInferenceRowCount {
			break
		}
	}

	details.NumRows = dataRowCount

	for i, header := range details.Headers {
		sampleStrings := make([]string, len(columnSamplesForDisplay[i]))
		copy(sampleStrings, columnSamplesForDisplay[i])

		if len(sampleStrings) > sampleDataRowCount {
			sampleStrings = sampleStrings[:sampleDataRowCount]
		}

		details.SampleData[header] = strings.Join(sampleStrings, ", ")
		details.DataTypes[header] = utils.InferDataType(columnSamplesForInference[i])
	}

	// Check if the number of headers and data types match
	if len(details.SampleData) != details.NumCols {
		c.logger.Error("Mismatch in number of headers and sample data",
			zap.Int("expected", details.NumCols),
			zap.Int("actual", len(details.SampleData)),
			zap.String("fileName", details.FileName),
		)

		return nil, appError.New(appError.InternalError, "mismatch in number of headers and sample data", nil)
	}

	if len(details.DataTypes) != details.NumCols {
		c.logger.Error("Mismatch in number of headers and data types",
			zap.Int("expected", details.NumCols),
			zap.Int("actual", len(details.DataTypes)),
			zap.String("fileName", details.FileName),
		)

		return nil, appError.New(appError.InternalError, "mismatch in number of headers and data types", nil)
	}

	headerDesc, err := c.getHeaderDescriptions(details)
	if err != nil {
		c.logger.Error("Failed to get header descriptions", zap.Error(err))
		return nil, appError.New(appError.InternalError, "failed to get header descriptions", err)
	}

	details.HeadersDescription = headerDesc

	return details, nil
}

// GetHeaderDescriptions retrieves the header descriptions from the CSV file, by sending a request to the Gemini API.
func (c *csvService) getHeaderDescriptions(details *CSVDetails) (map[string]string, error) {
	c.logger.Info("Getting Header Descriptions", zap.String("headers", details.FileName))

	genClient, err := provider.NewGeminiProvider(c.ctx)
	if err != nil {
		return nil, appError.New(appError.InternalError, "failed to create Gemini client", err)
	}
	defer genClient.Close()

	model := genClient.GenerativeModel(string(provider.Gemini1_5ProLatest))
	model.ResponseMIMEType = "application/json"

	dataTypesJson, _ := json.Marshal(details.DataTypes)
	sampleDataJson, _ := json.Marshal(details.SampleData)

	promptTemplate := `Context: 
	FileName: %s,
	Headers: %s
	HeadersCount: %d
	DataTypes: %s
	SampleData: %s
	NumCols: %d
	NumRows: %d

	Instructions:
	Given the Context, Which is a CSV File having headers and following data, we need to understand the meaning of each header.
	and how it related to the data present in the CSV file.

	Important:
	- Return the Description for Each Header as the HeadersCount Provided in the Context.
	- Dont change the order and the name of the headers. [Very Important]
	- Do not provide any other information, just the description of each header. [Very Important]
	- Provide the Description of each header in the CSV file, in a concise and clear manner, 
	- Maximum 50 words, Default: "No description available".
	- If unable to provide the description, please mention "No description available".
	- Make sure to provide the description in a single line, without any new lines or bullet points, try to be as concise as possible.
	
	Result:
	Provide the description in a JSON format, with the header as the key and the description as the value.

	Format:
	{
		"header1": "description1",
		"header2": "description2",
		"header3": "description3",
		...
	}

	Example:
	{
		"product_id": "Unique identifier for the product, used for tracking and inventory management.",
		"product_name": "Name of the product, used for display and identification purposes.",
		"product_price": "Price of the product, used for pricing and sales purposes, in USD and other currencies, [ values provided in String format, which is not normal ]",
		"product_description": "Description of the product, used for marketing and sales purposes.",
	}
	`

	prompt := fmt.Sprintf(promptTemplate, details.FileName, strings.Join(details.Headers, ", "), details.NumCols,
		string(dataTypesJson), string(sampleDataJson), details.NumCols, details.NumRows)

	resp, err := model.GenerateContent(c.ctx, genai.Text(prompt))
	if err != nil {
		c.logger.Error("Failed to generate header descriptions", zap.Error(err))
		return nil, appError.New(appError.InternalError, err.Error(), err)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		c.logger.Error("Gemini response was empty or missing content", zap.String("fileName", details.FileName))
		return nil, appError.New(appError.InternalError, "no header descriptions found in Gemini response", nil)
	}

	part := resp.Candidates[0].Content.Parts[0]
	rawJsonText, ok := part.(genai.Text)
	if !ok {
		c.logger.Sugar().Error("Unexpected response part type from Gemini", part, zap.String("fileName", details.FileName))
		return nil, appError.New(appError.InternalError, "unexpected response format from Gemini", nil)
	}

	// Convert the response part to a string, which is a JSON object
	// and remove any leading or trailing whitespace
	rawJson := string(rawJsonText)
	rawJson = strings.TrimSpace(rawJson)
	if strings.HasPrefix(rawJson, "```json") {
		rawJson = strings.TrimPrefix(rawJson, "```json")
		rawJson = strings.TrimSuffix(rawJson, "```")
		rawJson = strings.TrimSpace(rawJson)
	} else if strings.HasPrefix(rawJson, "```") {
		rawJson = strings.TrimPrefix(rawJson, "```")
		rawJson = strings.TrimSuffix(rawJson, "```")
		rawJson = strings.TrimSpace(rawJson)
	}

	// Unmarshal the JSON string into a map
	var headerDescriptions map[string]string

	err = json.Unmarshal([]byte(rawJson), &headerDescriptions)
	if err != nil {
		c.logger.Error("Failed to unmarshal header descriptions JSON from Gemini",
			zap.Error(err),
			zap.String("rawJson", rawJson),
			zap.String("fileName", details.FileName),
		)
		return nil, appError.New(appError.InternalError, "failed to parse descriptions from Gemini response", err)
	}

	if len(headerDescriptions) != details.NumCols {
		c.logger.Error("Mismatch in number of headers and descriptions",
			zap.Int("expected", details.NumCols),
			zap.Int("actual", len(headerDescriptions)),
			zap.String("fileName", details.FileName),
		)

		return nil, appError.New(appError.InternalError, "mismatch in number of headers and descriptions", nil)
	}

	return headerDescriptions, nil
}