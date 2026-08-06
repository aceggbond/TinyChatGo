plugins { id("com.android.application") }

android {
    namespace = "com.aceggbond.tinychatgo"
    compileSdk = 35
    defaultConfig {
        applicationId = "com.aceggbond.tinychatgo"
        minSdk = 26
        targetSdk = 35
        versionCode = 105
        versionName = "1.0.1"
    }
    buildTypes { release { isMinifyEnabled = false; signingConfig = signingConfigs.getByName("debug") } }
}
