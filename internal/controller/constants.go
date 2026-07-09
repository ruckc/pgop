/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// Common constants used across controllers
const (
	ReasonReconcileError = "ReconcileError"

	LabelAppName      = "app.kubernetes.io/name"
	LabelAppInstance  = "app.kubernetes.io/instance"
	LabelAppManagedBy = "app.kubernetes.io/managed-by"
	LabelValuePgop    = "pgop"

	AppNamePostgresql = "postgresql"

	SecretKeyUsername = "username"
	SecretKeyPassword = "password"
	SecretKeyHost     = "host"
	SecretKeyPort     = "port"
	SecretKeyDatabase = "database"

	DefaultPostgresImage    = "postgres:18"
	DefaultOperatorUsername = "pgop_operator"

	ConditionTypeAvailable = "Available"

	envAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"

	volPgbackrestConfig = "pgbackrest-config"
	volPgbackrestTmp    = "pgbackrest-tmp"
)
