package booth.runner

import com.fasterxml.jackson.databind.ObjectMapper
import com.fasterxml.jackson.module.scala.{ClassTagExtensions, DefaultScalaModule}

import org.apache.spark.sql.SparkSession

/** What the interpreter's prelude reads: the driver's SparkSession and the run's arguments. */
object Runtime {
  @volatile var spark: SparkSession = _
  @volatile var args: Array[String] = Array.empty
}

/** One booth-storage location a run asked for (BOOTH_STORAGE), with its s3a root. */
final case class StorageLocation(backendId: String, path: String, access: String, root: String)

/** `booth` in Scala code: what a Python run reads from its environment, typed, plus JSON for
  * booth-pipeline's Scala prelude (its params and inputs in, its output line out). Jackson and its
  * Scala module are Spark's own, already in the image.
  */
object Booth {
  private val mapper = {
    val m = new ObjectMapper() with ClassTagExtensions
    m.registerModule(DefaultScalaModule)
    m
  }

  /** The lakehouse catalog's name (BOOTH_LAKEHOUSE_CATALOG), "lakehouse" without data access. */
  def catalog: String = sys.env.getOrElse("BOOTH_LAKEHOUSE_CATALOG", "lakehouse")

  /** The storage locations the run asked for (BOOTH_STORAGE), in order; empty without any. */
  def storage: Seq[StorageLocation] = sys.env.get("BOOTH_STORAGE").map(storageFrom).getOrElse(Nil)

  private[runner] def storageFrom(raw: String): Seq[StorageLocation] =
    if (raw.trim.isEmpty) Nil else mapper.readValue[List[StorageLocation]](raw)

  /** value as JSON: Scala collections, case classes, options, numbers and strings as Jackson's
    * Scala module writes them.
    */
  def json(value: Any): String = mapper.writeValueAsString(value)

  /** JSON text as plain Scala values: Map[String, Any], List[Any], String, numbers, Boolean, null. */
  def parseJson(text: String): Any = mapper.readValue[Any](text)
}
