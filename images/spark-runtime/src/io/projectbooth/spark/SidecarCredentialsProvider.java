package io.projectbooth.spark;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.HashMap;
import java.util.Map;

import org.apache.hadoop.conf.Configuration;

import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.AwsCredentials;
import software.amazon.awssdk.auth.credentials.AwsCredentialsProvider;
import software.amazon.awssdk.auth.credentials.AwsSessionCredentials;

/**
 * S3A credentials from booth-core's credential sidecar (docs/design-v0.md item 4).
 *
 * <p>The sidecar keeps a standard AWS shared-credentials file current, renaming a new one over the
 * old on every lease renewal. Hadoop's own providers read such a file once; this one re-reads it
 * whenever it changes, so a run outlives its first lease (the credential-sidecar contract's "Known
 * limitation" for engines that aren't boto3-based).
 *
 * <p>Which file: {@code fs.s3a.booth.credentials.<scheme>} of the filesystem's own configuration,
 * which booth-spark sets per bucket ({@code fs.s3a.bucket.<bucket>.booth.credentials.<scheme>}, and
 * S3A applies a bucket's settings to that bucket's filesystem). The scheme tells two leases on one
 * bucket apart: the lakehouse warehouse is {@code s3://} (the locations Iceberg's catalog gives) and a
 * declared storage location {@code s3a://}.
 */
public final class SidecarCredentialsProvider implements AwsCredentialsProvider {
    /** Configuration key prefix naming the credentials file for a scheme. */
    public static final String FILE_KEY = "fs.s3a.booth.credentials.";

    private final Path file;
    private final String what;
    private long modified = Long.MIN_VALUE;
    private AwsCredentials current;

    public SidecarCredentialsProvider(URI uri, Configuration conf) {
        String scheme = uri != null && uri.getScheme() != null ? uri.getScheme() : "s3a";
        String bucket = uri != null && uri.getHost() != null ? uri.getHost() : "";
        this.what = scheme + "://" + bucket;
        String f = conf.getTrimmed(FILE_KEY + scheme, "");
        if (f.isEmpty()) {
            throw new IllegalArgumentException("booth-spark: this run has no credentials for " + what
                    + " (it isn't the workspace's warehouse or one of the run's declared storage locations)");
        }
        this.file = Paths.get(f);
    }

    @Override
    public synchronized AwsCredentials resolveCredentials() {
        try {
            long m = Files.getLastModifiedTime(file).toMillis();
            if (current == null || m != modified) {
                current = read(file);
                modified = m;
            }
            return current;
        } catch (IOException e) {
            throw new UncheckedIOException("booth-spark: reading the credentials for " + what + " from " + file, e);
        }
    }

    /** Reads the [default] profile of an AWS shared-credentials file. */
    static AwsCredentials read(Path file) throws IOException {
        Map<String, String> keys = new HashMap<>();
        boolean inDefault = false;
        for (String raw : Files.readAllLines(file, StandardCharsets.UTF_8)) {
            String line = raw.trim();
            if (line.isEmpty() || line.startsWith("#") || line.startsWith(";")) {
                continue;
            }
            if (line.startsWith("[")) {
                inDefault = line.equals("[default]");
                continue;
            }
            int eq = line.indexOf('=');
            if (inDefault && eq > 0) {
                keys.put(line.substring(0, eq).trim(), line.substring(eq + 1).trim());
            }
        }
        String id = keys.get("aws_access_key_id");
        String secret = keys.get("aws_secret_access_key");
        if (id == null || id.isEmpty() || secret == null || secret.isEmpty()) {
            throw new IOException("no [default] access key in " + file);
        }
        String session = keys.get("aws_session_token");
        if (session != null && !session.isEmpty()) {
            return AwsSessionCredentials.create(id, secret, session);
        }
        return AwsBasicCredentials.create(id, secret);
    }

    @Override
    public String toString() {
        return "SidecarCredentialsProvider(" + what + ")";
    }
}
