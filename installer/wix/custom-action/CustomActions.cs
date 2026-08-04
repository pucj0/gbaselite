using System;
using System.Collections.Generic;
using System.IO;
using System.ComponentModel;
using System.Diagnostics;
using System.Net;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Principal;
using System.ServiceProcess;
using System.Text;
using System.Web.Script.Serialization;
using WixToolset.Dtf.WindowsInstaller;

namespace GBaseLite.CustomActions
{
    public static class CustomActions
    {
        private sealed class InstallerData
        {
            public string ConfigPath { get; set; } = string.Empty;
            public string DataPath { get; set; } = string.Empty;
            public string LogPath { get; set; } = string.Empty;
            public string InstallPath { get; set; } = string.Empty;
            public string Port { get; set; } = "3307";
            public string Username { get; set; } = "root";
            public string Password { get; set; } = string.Empty;
            public bool Reinitialize { get; set; }
            public bool ReinitializeConfirmed { get; set; }
            public bool AutoStart { get; set; }
            public bool StartService { get; set; }
            public bool ApplyConfiguration { get; set; }
            public bool ConfigureJournals { get; set; }
            public bool AuditEnabled { get; set; }
            public bool BinlogEnabled { get; set; }
            public int AuditRetentionDays { get; set; } = 7;
            public int BinlogRetentionDays { get; set; } = 7;
        }

        [CustomAction]
        public static ActionResult LoadExistingConfigDefaults(Session session)
        {
            try
            {
                var configPath = session["GBASE_CONFIG_PATH"];
                if (string.IsNullOrWhiteSpace(configPath) || !File.Exists(configPath))
                {
                    return ActionResult.Success;
                }

                var port = ReadConfigValue(configPath, "port");
                var username = ReadConfigValue(configPath, "username");
                if (!string.IsNullOrWhiteSpace(port))
                {
                    session["GBASE_PORT"] = port;
                }
                if (!string.IsNullOrWhiteSpace(username))
                {
                    session["GBASE_ADMIN_USER"] = username;
                }
                SetCheckboxFromConfig(session, configPath, "audit", "enabled", "GBASE_AUDIT_ENABLED");
                SetCheckboxFromConfig(session, configPath, "binlog", "enabled", "GBASE_BINLOG_ENABLED");
                session["GBASE_AUDIT_RETENTION_DAYS"] = ReadRetentionDaysFromConfig(configPath, "audit", 7).ToString();
                session["GBASE_BINLOG_RETENTION_DAYS"] = ReadRetentionDaysFromConfig(configPath, "binlog", 7).ToString();
                return ActionResult.Success;
            }
            catch (Exception error)
            {
                session.Log("Unable to load existing GBaseLite configuration defaults: {0}", error.Message);
                return ActionResult.Success;
            }
        }

        [CustomAction]
        public static ActionResult PrepareInstallerData(Session session)
        {
            try
            {
                var payload = new InstallerData
                {
                    ConfigPath = session["GBASE_CONFIG_PATH"],
                    DataPath = session["GBASE_DATA_DIR"],
                    LogPath = session["GBASE_LOG_DIR"],
                    InstallPath = session["INSTALLFOLDER"],
                    Port = session["GBASE_PORT"],
                    Username = session["GBASE_ADMIN_USER"],
                    Password = session["GBASE_ADMIN_PASSWORD"],
                    Reinitialize = session["GBASE_REINITIALIZE"] == "1",
                    ReinitializeConfirmed = session["GBASE_REINITIALIZE_CONFIRMED"] == "1",
                    AutoStart = session["GBASE_AUTO_START"] == "1",
                    StartService = session["GBASE_START_SERVICE"] == "1",
                    ApplyConfiguration = session["GBASE_APPLY_CONFIGURATION"] == "1",
                    ConfigureJournals = session["GBASE_CONFIGURE_JOURNALS"] == "1",
                    AuditEnabled = session["GBASE_AUDIT_ENABLED"] == "1",
                    BinlogEnabled = session["GBASE_BINLOG_ENABLED"] == "1",
                    AuditRetentionDays = ParseRetentionDays(session["GBASE_AUDIT_RETENTION_DAYS"]),
                    BinlogRetentionDays = ParseRetentionDays(session["GBASE_BINLOG_RETENTION_DAYS"])
                };
                Validate(payload);
                var json = new JavaScriptSerializer().Serialize(payload);
                var encoded = Convert.ToBase64String(Encoding.UTF8.GetBytes(json));
                session["FinalizeInstallation"] = "payload=" + encoded;
                return ActionResult.Success;
            }
            catch (Exception error)
            {
                session.Log("GBaseLite installer configuration validation failed: {0}", error.Message);
                return ActionResult.Failure;
            }
        }

        [CustomAction]
        public static ActionResult ValidateSelectedPort(Session session)
        {
            session["GBASE_PORT_VALIDATION_ERROR"] = string.Empty;
            if (!int.TryParse(session["GBASE_PORT"], out var port) || port < 1 || port > 65535)
            {
                session["GBASE_PORT_VALIDATION_ERROR"] = "端口必须是 1 到 65535 之间的整数。";
                return ActionResult.Success;
            }

            try
            {
                foreach (var processId in GetTcpListenerProcessIds(port))
                {
                    Process? process = null;
                    try
                    {
                        process = Process.GetProcessById(processId);
                        if (IsGBaseLiteProcess(process))
                        {
                            session.Log("Port {0} is currently used by GBaseLite PID {1}; the installer may replace it.", port, processId);
                            continue;
                        }

                        session.Log("Port {0} is used by a non-GBaseLite process PID {1}.", port, processId);
                        session["GBASE_PORT_VALIDATION_ERROR"] =
                            string.Format("端口 {0} 已被其他程序占用。请选择其他端口，或先停止占用该端口的程序。", port);
                        return ActionResult.Success;
                    }
                    catch (ArgumentException)
                    {
                        // The listener exited between the TCP table query and process lookup.
                    }
                    finally
                    {
                        process?.Dispose();
                    }
                }
            }
            catch (Exception error)
            {
                session.Log("Unable to validate port {0}: {1}", port, error.Message);
                session["GBASE_PORT_VALIDATION_ERROR"] =
                    string.Format("无法检查端口 {0} 是否可用。请以管理员身份重新运行安装程序。", port);
            }
            return ActionResult.Success;
        }

        [CustomAction]
        public static ActionResult FinalizeInstallation(Session session)
        {
            string? backupDataPath = null;
            byte[]? previousConfig = null;
            string? configPath = null;
            try
            {
                var encoded = session.CustomActionData["payload"];
                var json = Encoding.UTF8.GetString(Convert.FromBase64String(encoded));
                var payload = new JavaScriptSerializer().Deserialize<InstallerData>(json);
                Validate(payload);
                configPath = Path.GetFullPath(payload.ConfigPath);

                if (File.Exists(configPath))
                {
                    previousConfig = File.ReadAllBytes(configPath);
                }

                if (payload.Reinitialize)
                {
                    if (!payload.ReinitializeConfirmed)
                    {
                        throw new InvalidOperationException("Data reinitialization was not confirmed.");
                    }
                    var dataPath = Path.GetFullPath(payload.DataPath);
                    if (Directory.Exists(dataPath))
                    {
                        backupDataPath = dataPath.TrimEnd(Path.DirectorySeparatorChar) +
                            ".gbaselite-backup-" + Guid.NewGuid().ToString("N");
                        Directory.Move(dataPath, backupDataPath);
                    }
                    Directory.CreateDirectory(dataPath);
                }

                Directory.CreateDirectory(Path.GetDirectoryName(configPath));
                Directory.CreateDirectory(payload.DataPath);
                Directory.CreateDirectory(payload.LogPath);
                if (!File.Exists(configPath) || payload.Reinitialize || payload.ApplyConfiguration)
                {
                    if (File.Exists(configPath) && string.IsNullOrEmpty(payload.Password))
                    {
                        payload.Password = ReadConfigValue(configPath, "password");
                    }
                    WriteConfigAtomically(configPath, payload);
                }

				HardenRuntimePermissions(configPath, payload.DataPath, payload.LogPath);

                StopExistingWindowsService(session);
                StopStandaloneGBaseLiteListener(session, int.Parse(payload.Port));
                ConfigureServiceStartType(payload.AutoStart);

                if (payload.StartService)
                {
                    try
                    {
                        using (var service = new ServiceController("GBaseLite"))
                        {
                            if (service.Status != ServiceControllerStatus.Running &&
                                service.Status != ServiceControllerStatus.StartPending)
                            {
                                service.Start();
                                session.Log("Requested GBaseLite Windows service startup.");
                            }
                        }
                    }
                    catch (Exception error)
                    {
                        session.Log("GBaseLite service was installed but could not be started: {0}", error.Message);
                    }
                }

                if (backupDataPath != null)
                {
                    // Deleting a large prior data directory can hold the MSI progress UI for minutes.
                    // Keep the adjacent backup so a successful reinitialization remains recoverable.
                    session.Log("Retained the previous GBaseLite data directory backup after reinitialization.");
                }
                return ActionResult.Success;
            }
            catch (Exception error)
            {
                session.Log("GBaseLite installer finalization failed: {0}", error.Message);
                try
                {
                    if (configPath != null)
                    {
                        if (previousConfig != null)
                        {
                            File.WriteAllBytes(configPath, previousConfig);
                        }
                        else if (File.Exists(configPath))
                        {
                            File.Delete(configPath);
                        }
                    }
                    if (backupDataPath != null && Directory.Exists(backupDataPath))
                    {
                        var originalDataPath = backupDataPath.Substring(0, backupDataPath.IndexOf(".gbaselite-backup-", StringComparison.Ordinal));
                        if (Directory.Exists(originalDataPath))
                        {
                            Directory.Delete(originalDataPath, true);
                        }
                        Directory.Move(backupDataPath, originalDataPath);
                    }
                }
                catch
                {
                    // Avoid logging sensitive paths or masking the original installer failure.
                }
                return ActionResult.Failure;
            }
        }

        private static void Validate(InstallerData payload)
        {
            if (!int.TryParse(payload.Port, out var port) || port < 1 || port > 65535)
            {
                throw new InvalidOperationException("Port must be between 1 and 65535.");
            }
            if (string.IsNullOrWhiteSpace(payload.Username))
            {
                throw new InvalidOperationException("Administrator username is required.");
            }
            var existingConfigCanBePreserved = File.Exists(payload.ConfigPath) && !payload.Reinitialize;
            if (string.IsNullOrEmpty(payload.Password) && !existingConfigCanBePreserved)
            {
                throw new InvalidOperationException("Administrator password is required.");
            }
			ValidateRuntimeDirectory(payload.DataPath, payload.InstallPath, "Data directory");
			ValidateRuntimeDirectory(payload.LogPath, payload.InstallPath, "Log directory");
            if (payload.Reinitialize && !payload.ReinitializeConfirmed)
            {
                throw new InvalidOperationException("Data reinitialization requires confirmation.");
            }
            ValidateRetentionDays(payload.AuditRetentionDays);
            ValidateRetentionDays(payload.BinlogRetentionDays);
        }

		private static void ValidateRuntimeDirectory(string value, string installPath, string description)
		{
			if (string.IsNullOrWhiteSpace(value) || !Path.IsPathRooted(value))
			{
				throw new InvalidOperationException(description + " must be an absolute path.");
			}
            var dataPath = Path.GetFullPath(value).TrimEnd(Path.DirectorySeparatorChar);
            var root = Path.GetPathRoot(dataPath)?.TrimEnd(Path.DirectorySeparatorChar);
			if (string.Equals(dataPath, root, StringComparison.OrdinalIgnoreCase))
			{
				throw new InvalidOperationException("A drive root cannot be used as the " + description.ToLowerInvariant() + ".");
            }
            var protectedPaths = new[]
            {
                Environment.GetFolderPath(Environment.SpecialFolder.Windows),
                Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles),
                Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData),
                installPath
            };
            foreach (var protectedPath in protectedPaths)
            {
                if (!string.IsNullOrWhiteSpace(protectedPath) &&
                    string.Equals(dataPath, Path.GetFullPath(protectedPath).TrimEnd(Path.DirectorySeparatorChar), StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException("The selected directory is reserved and cannot be reinitialized.");
                }
            }
        }

        private static void WriteConfigAtomically(string path, InstallerData payload)
        {
            var temporaryPath = path + ".tmp-" + Guid.NewGuid().ToString("N");
			var serverHost = "127.0.0.1";
			var loginFailureLimit = "5";
			var loginFailureWindowSeconds = "60";
			var loginFailureBlockSeconds = "30";
			var tlsEnabled = "false";
			var tlsCertFile = string.Empty;
			var tlsKeyFile = string.Empty;
			var requireSecureTransport = "false";
			var logMaxSizeMB = "20";
			var logRetentionDays = "7";
            var auditEnabled = payload.AuditEnabled ? "true" : "false";
            var auditRetentionDays = payload.AuditRetentionDays;
            var auditPath = Path.Combine(payload.LogPath, "audit.jsonl");
            var binlogEnabled = payload.BinlogEnabled ? "true" : "false";
            var binlogRetentionDays = payload.BinlogRetentionDays;
            var binlogPath = Path.Combine(payload.DataPath, "binlog.jsonl");
            if (File.Exists(path))
            {
				serverHost = ReadConfigSectionValue(path, "server", "host", serverHost);
				loginFailureLimit = ReadConfigSectionValue(path, "security", "login_failure_limit", loginFailureLimit);
				loginFailureWindowSeconds = ReadConfigSectionValue(path, "security", "login_failure_window_seconds", loginFailureWindowSeconds);
				loginFailureBlockSeconds = ReadConfigSectionValue(path, "security", "login_failure_block_seconds", loginFailureBlockSeconds);
				tlsEnabled = ReadConfigSectionValue(path, "tls", "enabled", tlsEnabled);
				tlsCertFile = ReadConfigSectionValue(path, "tls", "cert_file", tlsCertFile);
				tlsKeyFile = ReadConfigSectionValue(path, "tls", "key_file", tlsKeyFile);
				requireSecureTransport = ReadConfigSectionValue(path, "tls", "require_secure_transport", requireSecureTransport);
				logMaxSizeMB = ReadConfigSectionValue(path, "log", "max_size_mb", logMaxSizeMB);
				logRetentionDays = ReadConfigSectionValue(path, "log", "retention_days", logRetentionDays);
                auditPath = ReadConfigSectionValue(path, "audit", "path", auditPath);
                binlogPath = ReadConfigSectionValue(path, "binlog", "path", binlogPath);
                if (!payload.ConfigureJournals)
                {
                    auditEnabled = ReadConfigSectionValue(path, "audit", "enabled", auditEnabled);
                    binlogEnabled = ReadConfigSectionValue(path, "binlog", "enabled", binlogEnabled);
                    auditRetentionDays = ReadRetentionDaysFromConfig(path, "audit", auditRetentionDays);
                    binlogRetentionDays = ReadRetentionDaysFromConfig(path, "binlog", binlogRetentionDays);
                }
            }
            var contents =
                "server:\r\n" +
				"  host: " + serverHost + "\r\n" +
                "  port: " + payload.Port + "\r\n\r\n" +
                "storage:\r\n" +
                "  path: '" + EscapeSingleQuoted(payload.DataPath) + "'\r\n\r\n" +
                "auth:\r\n" +
                "  username: '" + EscapeSingleQuoted(payload.Username) + "'\r\n" +
                "  password: '" + EscapeSingleQuoted(payload.Password) + "'\r\n\r\n" +
				"security:\r\n" +
				"  login_failure_limit: " + loginFailureLimit + "\r\n" +
				"  login_failure_window_seconds: " + loginFailureWindowSeconds + "\r\n" +
				"  login_failure_block_seconds: " + loginFailureBlockSeconds + "\r\n\r\n" +
				"tls:\r\n" +
				"  enabled: " + tlsEnabled + "\r\n" +
				"  cert_file: '" + EscapeSingleQuoted(tlsCertFile) + "'\r\n" +
				"  key_file: '" + EscapeSingleQuoted(tlsKeyFile) + "'\r\n" +
				"  require_secure_transport: " + requireSecureTransport + "\r\n\r\n" +
                "log:\r\n" +
                "  path: '" + EscapeSingleQuoted(payload.LogPath) + "'\r\n" +
				"  max_size_mb: " + logMaxSizeMB + "\r\n" +
				"  retention_days: " + logRetentionDays + "\r\n\r\n" +
                "audit:\r\n" +
                "  enabled: " + auditEnabled + "\r\n" +
                "  path: '" + EscapeSingleQuoted(auditPath) + "'\r\n" +
                "  retention_days: " + auditRetentionDays + "\r\n\r\n" +
                "binlog:\r\n" +
                "  enabled: " + binlogEnabled + "\r\n" +
                "  path: '" + EscapeSingleQuoted(binlogPath) + "'\r\n" +
                "  retention_days: " + binlogRetentionDays + "\r\n";
            File.WriteAllText(temporaryPath, contents, new UTF8Encoding(false));
            if (File.Exists(path))
            {
                File.Replace(temporaryPath, path, null);
            }
            else
            {
                File.Move(temporaryPath, path);
            }
        }

		private static void HardenRuntimePermissions(string configPath, string dataPath, string logPath)
		{
			var system = new SecurityIdentifier(WellKnownSidType.LocalSystemSid, null);
			var administrators = new SecurityIdentifier(WellKnownSidType.BuiltinAdministratorsSid, null);

			var configSecurity = new FileSecurity();
			configSecurity.SetAccessRuleProtection(true, false);
			configSecurity.SetOwner(system);
			configSecurity.AddAccessRule(new FileSystemAccessRule(system, FileSystemRights.FullControl, AccessControlType.Allow));
			configSecurity.AddAccessRule(new FileSystemAccessRule(administrators, FileSystemRights.FullControl, AccessControlType.Allow));
			File.SetAccessControl(configPath, configSecurity);

			HardenDirectoryPermissions(dataPath, system, administrators);
			if (!string.Equals(Path.GetFullPath(dataPath), Path.GetFullPath(logPath), StringComparison.OrdinalIgnoreCase))
			{
				HardenDirectoryPermissions(logPath, system, administrators);
			}
		}

		private static void HardenDirectoryPermissions(string path, SecurityIdentifier system, SecurityIdentifier administrators)
		{
			var security = new DirectorySecurity();
			security.SetAccessRuleProtection(true, false);
			security.SetOwner(system);
			const InheritanceFlags inheritance = InheritanceFlags.ContainerInherit | InheritanceFlags.ObjectInherit;
			security.AddAccessRule(new FileSystemAccessRule(system, FileSystemRights.FullControl, inheritance, PropagationFlags.None, AccessControlType.Allow));
			security.AddAccessRule(new FileSystemAccessRule(administrators, FileSystemRights.FullControl, inheritance, PropagationFlags.None, AccessControlType.Allow));
			Directory.SetAccessControl(path, security);
		}

        private static string EscapeSingleQuoted(string value)
        {
            return value.Replace("'", "''");
        }

        private static string ReadConfigValue(string path, string key)
        {
            foreach (var line in File.ReadLines(path))
            {
                var trimmed = line.Trim();
                if (!trimmed.StartsWith(key + ":", StringComparison.Ordinal))
                {
                    continue;
                }
                var value = trimmed.Substring(key.Length + 1).Trim();
                if (value.Length >= 2 && value[0] == '\'' && value[value.Length - 1] == '\'')
                {
                    value = value.Substring(1, value.Length - 2).Replace("''", "'");
                }
                return value;
            }
            return string.Empty;
        }

        private static string ReadConfigSectionValue(string path, string section, string key, string fallback)
        {
            var inSection = false;
            foreach (var line in File.ReadLines(path))
            {
                if (string.IsNullOrWhiteSpace(line) || line.TrimStart().StartsWith("#", StringComparison.Ordinal))
                {
                    continue;
                }

                var indentation = line.Length - line.TrimStart().Length;
                var trimmed = line.Trim();
                if (indentation == 0)
                {
                    inSection = string.Equals(trimmed, section + ":", StringComparison.Ordinal);
                    continue;
                }
                if (!inSection || !trimmed.StartsWith(key + ":", StringComparison.Ordinal))
                {
                    continue;
                }

                var value = trimmed.Substring(key.Length + 1).Trim();
                if (value.Length >= 2 && value[0] == '\'' && value[value.Length - 1] == '\'')
                {
                    value = value.Substring(1, value.Length - 2).Replace("''", "'");
                }
                return string.IsNullOrWhiteSpace(value) ? fallback : value;
            }
            return fallback;
        }

        private static void SetCheckboxFromConfig(Session session, string path, string section, string key, string property)
        {
            var value = ReadConfigSectionValue(path, section, key, "false");
            session[property] = string.Equals(value, "true", StringComparison.OrdinalIgnoreCase) || value == "1"
                ? "1"
                : string.Empty;
        }

        private static int ReadRetentionDaysFromConfig(string path, string section, int fallback)
        {
            var value = ReadConfigSectionValue(path, section, "retention_days", fallback.ToString());
            return int.TryParse(value, out var days) && days >= 0 && days <= 365 ? days : fallback;
        }

        private static int ParseRetentionDays(string value)
        {
            if (!int.TryParse(value, out var days))
            {
                throw new InvalidOperationException("Log retention days must be an integer.");
            }
            ValidateRetentionDays(days);
            return days;
        }

        private static void ValidateRetentionDays(int days)
        {
            if (days < 0 || days > 365)
            {
                throw new InvalidOperationException("Log retention days must be 0 or between 1 and 365.");
            }
        }

        private static void StopExistingWindowsService(Session session)
        {
            try
            {
                using (var service = new ServiceController("GBaseLite"))
                {
                    service.Refresh();
                    if (service.Status == ServiceControllerStatus.Stopped)
                    {
                        return;
                    }
                    if (service.Status == ServiceControllerStatus.StopPending)
                    {
                        service.WaitForStatus(ServiceControllerStatus.Stopped, TimeSpan.FromSeconds(30));
                        return;
                    }
                    if (service.CanStop)
                    {
                        session.Log("Stopping the existing GBaseLite Windows service.");
                        service.Stop();
                        service.WaitForStatus(ServiceControllerStatus.Stopped, TimeSpan.FromSeconds(30));
                    }
                }
            }
            catch (InvalidOperationException error)
            {
                session.Log("No stoppable existing GBaseLite Windows service was found: {0}", error.Message);
            }
        }

        private static void StopStandaloneGBaseLiteListener(Session session, int port)
        {
            foreach (var processId in GetTcpListenerProcessIds(port))
            {
                Process? process = null;
                try
                {
                    process = Process.GetProcessById(processId);
                    if (!IsGBaseLiteProcess(process))
                    {
                        session.Log("Port {0} is used by a non-GBaseLite process (PID {1}); the installer will not stop it.", port, processId);
                        continue;
                    }

                    session.Log("Stopping standalone GBaseLite process PID {0} listening on port {1}.", processId, port);
                    if (!process.HasExited)
                    {
                        process.Kill();
                        process.WaitForExit(10000);
                    }
                    if (!process.HasExited)
                    {
                        session.Log("Standalone GBaseLite process PID {0} did not exit within 10 seconds.", processId);
                    }
                }
                catch (ArgumentException)
                {
                    // The listener exited between the TCP table query and process lookup.
                }
                catch (Exception error)
                {
                    session.Log("Unable to stop standalone GBaseLite process PID {0}: {1}", processId, error.Message);
                }
                finally
                {
                    process?.Dispose();
                }
            }
        }

        private static bool IsGBaseLiteProcess(Process process)
        {
            return string.Equals(process.ProcessName, "gbaselite", StringComparison.OrdinalIgnoreCase);
        }

        private static IReadOnlyCollection<int> GetTcpListenerProcessIds(int port)
        {
            const int addressFamilyInet = 2;
            const int tcpTableOwnerPidListener = 3;
            const uint errorInsufficientBuffer = 122;

            var bufferSize = 0;
            var result = GetExtendedTcpTable(IntPtr.Zero, ref bufferSize, false, addressFamilyInet, tcpTableOwnerPidListener, 0);
            if (result != errorInsufficientBuffer)
            {
                if (result == 0)
                {
                    return Array.Empty<int>();
                }
                throw new Win32Exception((int)result, "Unable to inspect active TCP listeners.");
            }

            var buffer = Marshal.AllocHGlobal(bufferSize);
            try
            {
                result = GetExtendedTcpTable(buffer, ref bufferSize, false, addressFamilyInet, tcpTableOwnerPidListener, 0);
                if (result != 0)
                {
                    throw new Win32Exception((int)result, "Unable to inspect active TCP listeners.");
                }

                var processIds = new HashSet<int>();
                var rowCount = Marshal.ReadInt32(buffer);
                var rowSize = Marshal.SizeOf(typeof(MibTcpRowOwnerPid));
                var rowPointer = IntPtr.Add(buffer, sizeof(int));
                for (var index = 0; index < rowCount; index++)
                {
                    var row = Marshal.PtrToStructure<MibTcpRowOwnerPid>(rowPointer);
                    var localPort = unchecked((ushort)IPAddress.NetworkToHostOrder((short)row.LocalPort));
                    if (localPort == port)
                    {
                        processIds.Add(unchecked((int)row.OwningProcessId));
                    }
                    rowPointer = IntPtr.Add(rowPointer, rowSize);
                }
                return processIds;
            }
            finally
            {
                Marshal.FreeHGlobal(buffer);
            }
        }

        private static void ConfigureServiceStartType(bool autoStart)
        {
            const uint scManagerConnect = 0x0001;
            const uint serviceChangeConfig = 0x0002;
            const uint serviceNoChange = 0xFFFFFFFF;
            const uint serviceAutoStart = 2;
            const uint serviceDemandStart = 3;

            var serviceManager = OpenSCManager(null, null, scManagerConnect);
            if (serviceManager == IntPtr.Zero)
            {
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Unable to open the Windows Service Control Manager.");
            }
            try
            {
                var service = OpenService(serviceManager, "GBaseLite", serviceChangeConfig);
                if (service == IntPtr.Zero)
                {
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "Unable to open the GBaseLite Windows service.");
                }
                try
                {
                    if (!ChangeServiceConfig(
                        service,
                        serviceNoChange,
                        autoStart ? serviceAutoStart : serviceDemandStart,
                        serviceNoChange,
                        null,
                        null,
                        IntPtr.Zero,
                        null,
                        null,
                        null,
                        null))
                    {
                        throw new Win32Exception(Marshal.GetLastWin32Error(), "Unable to configure the GBaseLite service start type.");
                    }
                }
                finally
                {
                    CloseServiceHandle(service);
                }
            }
            finally
            {
                CloseServiceHandle(serviceManager);
            }
        }

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern IntPtr OpenSCManager(string? machineName, string? databaseName, uint desiredAccess);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern IntPtr OpenService(IntPtr serviceManager, string serviceName, uint desiredAccess);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool ChangeServiceConfig(
            IntPtr service,
            uint serviceType,
            uint startType,
            uint errorControl,
            string? binaryPathName,
            string? loadOrderGroup,
            IntPtr tagId,
            string? dependencies,
            string? serviceStartName,
            string? password,
            string? displayName);

        [DllImport("advapi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool CloseServiceHandle(IntPtr serviceHandle);

        [StructLayout(LayoutKind.Sequential)]
        private struct MibTcpRowOwnerPid
        {
            public uint State;
            public uint LocalAddress;
            public uint LocalPort;
            public uint RemoteAddress;
            public uint RemotePort;
            public uint OwningProcessId;
        }

        [DllImport("iphlpapi.dll", SetLastError = true)]
        private static extern uint GetExtendedTcpTable(
            IntPtr tcpTable,
            ref int size,
            [MarshalAs(UnmanagedType.Bool)] bool order,
            int addressFamily,
            int tableClass,
            uint reserved);
    }
}
