package handler

// 管理面（/admin）：排除 user tag；生成到本包。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -exclude-tags user -package handler -o api.gen.go ../../openapi/openapi.yaml
// 用户面（/user）：仅 user tag；独立包（共享 schema 类型在各自包内不冲突）。
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags user -package user -o user/api.gen.go ../../openapi/openapi.yaml
