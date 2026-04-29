package main

import (
	"os"
	"strings"
)

var (
	CloudStoreEnabled bool
	S3BucketName      string
	AwsRegionName     string
	SupabaseUrl       string
	SupabaseAnonKey   string
)

func loadConfig() {
	val := strings.ToUpper(os.Getenv("CLOUD_STORE"))
	CloudStoreEnabled = val == "TRUE" || val == "YES" || val == "ACTIVE"

	S3BucketName = os.Getenv("S3_BUCKET_NAME")

	AwsRegionName = os.Getenv("AWS_REGION_NAME")
	if AwsRegionName == "" {
		AwsRegionName = os.Getenv("AWS_REGION")
	}

	SupabaseUrl = os.Getenv("NEXT_PUBLIC_SUPABASE_URL")
	SupabaseAnonKey = os.Getenv("NEXT_PUBLIC_SUPABASE_ANON_KEY")
}
