package datatransfer

import (
	"time"

	dtpb "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The data sources served: scheduled queries and dataset copies. Every
// other data source (Google Ads, Cloud Storage, Amazon S3 and the rest)
// reaches a third-party system and is NOT_FOUND here.
const (
	ScheduledQuery   = "scheduled_query"
	CrossRegionCopy  = "cross_region_copy"
	defaultSchedule  = "every 24 hours"
	queryMinInterval = 5 * time.Minute
	copyMinInterval  = 12 * time.Hour
)

// dataSources are the two definitions. The parameters and minimum
// intervals are Google's documented ones (scheduling-queries,
// copying-datasets); the description texts and update deadline are ours,
// not observed.
var dataSources = map[string]*dtpb.DataSource{
	ScheduledQuery: {
		DataSourceId: ScheduledQuery, DisplayName: "Scheduled Query",
		Description:  "Scheduled queries run a GoogleSQL query on a recurring schedule and can write the results to a destination table.",
		TransferType: dtpb.TransferType_BATCH, SupportsMultipleTransfers: true, UpdateDeadlineSeconds: 21600,
		DefaultSchedule: defaultSchedule, SupportsCustomSchedule: true,
		HelpUrl:                 "https://cloud.google.com/bigquery/docs/scheduling-queries",
		AuthorizationType:       dtpb.DataSource_AUTHORIZATION_TYPE_UNSPECIFIED,
		DataRefreshType:         dtpb.DataSource_DATA_REFRESH_TYPE_UNSPECIFIED,
		MinimumScheduleInterval: durationpb.New(queryMinInterval),
		Parameters: []*dtpb.DataSourceParameter{
			{ParamId: "query", DisplayName: "Query string", Description: "The GoogleSQL query to run.", Type: dtpb.DataSourceParameter_STRING, Required: true},
			{ParamId: "destination_table_name_template", DisplayName: "Destination table name", Description: "The table the results are written to; {run_date} and {run_time|\"%Y%m%d\"} are templated.", Type: dtpb.DataSourceParameter_STRING},
			{ParamId: "write_disposition", DisplayName: "Write preference", Description: "WRITE_TRUNCATE overwrites the table, WRITE_APPEND appends to it.", Type: dtpb.DataSourceParameter_STRING, AllowedValues: []string{"WRITE_TRUNCATE", "WRITE_APPEND"}},
			{ParamId: "partitioning_field", DisplayName: "Partitioning field", Description: "The column the destination table is time-unit partitioned on.", Type: dtpb.DataSourceParameter_STRING},
		},
	},
	CrossRegionCopy: {
		DataSourceId: CrossRegionCopy, DisplayName: "Dataset Copy",
		Description:  "Copies the tables of a dataset into another dataset, within or across regions.",
		TransferType: dtpb.TransferType_BATCH, SupportsMultipleTransfers: true, UpdateDeadlineSeconds: 86400,
		DefaultSchedule: defaultSchedule, SupportsCustomSchedule: true,
		HelpUrl:                 "https://cloud.google.com/bigquery/docs/copying-datasets",
		MinimumScheduleInterval: durationpb.New(copyMinInterval),
		Parameters: []*dtpb.DataSourceParameter{
			{ParamId: "source_project_id", DisplayName: "Source project", Description: "The project that holds the source dataset.", Type: dtpb.DataSourceParameter_STRING, Required: true},
			{ParamId: "source_dataset_id", DisplayName: "Source dataset", Description: "The dataset whose tables are copied.", Type: dtpb.DataSourceParameter_STRING, Required: true},
			{ParamId: "overwrite_destination_table", DisplayName: "Overwrite destination table", Description: "Truncate the tables of a previous copy and refresh all data.", Type: dtpb.DataSourceParameter_BOOLEAN},
		},
	},
}

// dataSourceIDs is the listing order.
var dataSourceIDs = []string{CrossRegionCopy, ScheduledQuery}

func dataSource(locationName, id string) *dtpb.DataSource {
	d := proto.Clone(dataSources[id]).(*dtpb.DataSource)
	d.Name = locationName + "/dataSources/" + id
	return d
}
