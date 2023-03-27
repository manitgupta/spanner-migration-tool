import { Interface } from "readline"

export default interface IDbConfig {
  dbEngine: string
  isSharded: string
  hostName: string
  port: string
  userName: string
  password: string
  dbName: string
}

export interface IDbConfigs {
  dbConfigs: Array<IDbConfig>
  isRestoredSession: string
}

export interface IShardSessionDetails {
  sourceDatabaseEngine: string
  isRestoredSession: string
}