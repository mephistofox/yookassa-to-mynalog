package receipts

import "github.com/mephistofox/yookassa-to-mynalog/pkg/sqlitestate"

type Store = sqlitestate.Store

var OpenStore = sqlitestate.Open
