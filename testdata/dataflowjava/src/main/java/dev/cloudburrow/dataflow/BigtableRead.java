package dev.cloudburrow.dataflow;

import com.google.bigtable.v2.Cell;
import com.google.bigtable.v2.Column;
import com.google.bigtable.v2.Family;
import com.google.bigtable.v2.Row;
import org.apache.beam.sdk.Pipeline;
import org.apache.beam.sdk.io.TextIO;
import org.apache.beam.sdk.io.gcp.bigtable.BigtableIO;
import org.apache.beam.sdk.options.Description;
import org.apache.beam.sdk.options.PipelineOptions;
import org.apache.beam.sdk.options.PipelineOptionsFactory;
import org.apache.beam.sdk.transforms.MapElements;
import org.apache.beam.sdk.values.TypeDescriptors;

/**
 * Reads a Bigtable table with Beam's BigtableIO and writes each row's key
 * and cells (#2215), submitted with the Java SDK's DataflowRunner.
 */
public class BigtableRead {
  public interface Options extends PipelineOptions {
    @Description("Bigtable project")
    String getBigtableProject();

    void setBigtableProject(String v);

    @Description("Bigtable instance")
    String getBigtableInstance();

    void setBigtableInstance(String v);

    @Description("Bigtable table")
    String getBigtableTable();

    void setBigtableTable(String v);

    @Description("Output prefix")
    String getOutput();

    void setOutput(String v);
  }

  static String format(Row row) {
    StringBuilder b = new StringBuilder(row.getKey().toStringUtf8());
    for (Family f : row.getFamiliesList()) {
      for (Column c : f.getColumnsList()) {
        for (Cell cell : c.getCellsList()) {
          b.append(' ').append(f.getName()).append(':').append(c.getQualifier().toStringUtf8())
              .append('=').append(cell.getValue().toStringUtf8());
        }
      }
    }
    return b.toString();
  }

  public static void main(String[] args) {
    Options options = PipelineOptionsFactory.fromArgs(args).withValidation().as(Options.class);
    Pipeline p = Pipeline.create(options);
    p.apply("ReadBigtable", BigtableIO.read().withProjectId(options.getBigtableProject())
            .withInstanceId(options.getBigtableInstance()).withTableId(options.getBigtableTable()))
        .apply("Format", MapElements.into(TypeDescriptors.strings()).via(BigtableRead::format))
        .apply("Write", TextIO.write().to(options.getOutput()).withoutSharding());
    p.run();
  }
}
